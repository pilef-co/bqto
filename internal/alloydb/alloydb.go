package alloydb

import (
	"github.com/pilef-co/bqto/gcperr"

	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pilef-co/bqto/quota"
	"google.golang.org/api/alloydb/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
)

var clientOptions []option.ClientOption

type Endpoint struct {
	IP             string
	Port           int
	Region         string
	NetworkProject string
	Network        string
}

const (
	defaultPort = 5432
	poolerPort  = 6432
)

type Choice struct {
	Value string
	Label string
}

func List(ctx context.Context, project string) ([]string, error) {
	choices, err := Choices(ctx, project)
	if err != nil {
		return nil, err
	}
	refs := make([]string, len(choices))
	for i, c := range choices {
		refs[i] = c.Value
	}
	return refs, nil
}

func Choices(ctx context.Context, project string) ([]Choice, error) {
	svc, err := alloydb.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, gcperr.Wrap("connect to the AlloyDB API", err)
	}
	parent := fmt.Sprintf("projects/%s/locations/-/clusters/-", project)
	choices, err := gcperr.Retry(ctx, func() ([]Choice, error) {
		var choices []Choice
		err := svc.Projects.Locations.Clusters.Instances.List(parent).Pages(ctx, func(resp *alloydb.ListInstancesResponse) error {
			for _, inst := range resp.Instances {
				c, choiceErr := choiceFor(inst)
				if choiceErr != nil {
					continue
				}
				choices = append(choices, c)
			}
			return nil
		})
		return choices, err
	})
	if err != nil {
		return nil, gcperr.Wrap("list AlloyDB instances in project "+project, err)
	}
	slices.SortFunc(choices, func(a, b Choice) int { return strings.Compare(a.Value, b.Value) })
	return choices, nil
}

func choiceFor(inst *alloydb.Instance) (Choice, error) {
	ref, err := refFrom(inst.Name)
	if err != nil {
		return Choice{}, err
	}
	c := Choice{Value: ref, Label: ref}
	switch inst.InstanceType {
	case "READ_POOL":
		c.Label = ref + " (read pool)"
	case "SECONDARY":
		c.Label = ref + " (cross-region replica)"
	}
	return c, nil
}

func Resolve(ctx context.Context, project, ref string) (Endpoint, error) {
	svc, err := alloydb.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return Endpoint{}, gcperr.Wrap("connect to the AlloyDB API", err)
	}
	full, err := resolveRef(ctx, project, ref)
	if err != nil {
		return Endpoint{}, err
	}
	inst, err := gcperr.RetryDo(ctx, svc.Projects.Locations.Clusters.Instances.Get(full).Context(ctx).Do)
	if err != nil {
		return Endpoint{}, gcperr.Get("AlloyDB instance", ref, project, ListCmd(project), err)
	}
	cluster, err := gcperr.RetryDo(ctx, svc.Projects.Locations.Clusters.Get(clusterOf(full)).Context(ctx).Do)
	if err != nil {
		return Endpoint{}, gcperr.Get("AlloyDB cluster", clusterRef(ref), project, ListCmd(project), err)
	}
	ep, err := endpointFrom(inst, cluster)
	if err != nil {
		return Endpoint{}, err
	}
	ep.NetworkProject, err = projectID(ctx, ep.NetworkProject)
	if err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

func resolveRef(ctx context.Context, project, ref string) (string, error) {
	if region, cluster, instance, ok := splitRef(ref); ok {
		return fmt.Sprintf("projects/%s/locations/%s/clusters/%s/instances/%s", project, region, cluster, instance), nil
	}
	refs, err := List(ctx, project)
	if err != nil {
		return "", err
	}
	matches := matchRefs(refs, ref)
	switch len(matches) {
	case 1:
		region, cluster, instance, _ := splitRef(matches[0])
		return fmt.Sprintf("projects/%s/locations/%s/clusters/%s/instances/%s", project, region, cluster, instance), nil
	case 0:
		return "", gcperr.Missing("AlloyDB instance", ref, project, ListCmd(project))
	}
	return "", fmt.Errorf("%q matches several instances (%s)\nFix: use REGION/CLUSTER/INSTANCE", ref, strings.Join(matches, ", "))
}

func matchRefs(refs []string, ref string) []string {
	var matches []string
	for _, r := range refs {
		if r == ref || strings.HasSuffix(r, "/"+ref) {
			matches = append(matches, r)
		}
	}
	return matches
}

func splitRef(ref string) (region, cluster, instance string, ok bool) {
	parts := strings.Split(ref, "/")
	if len(parts) != 3 || slices.Contains(parts, "") {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func refFrom(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 8 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "clusters" || parts[6] != "instances" {
		return "", fmt.Errorf("unexpected instance name %q", name)
	}
	return parts[3] + "/" + parts[5] + "/" + parts[7], nil
}

func clusterOf(instanceName string) string {
	return instanceName[:strings.LastIndex(instanceName, "/instances/")]
}

func endpointFrom(inst *alloydb.Instance, cluster *alloydb.Cluster) (Endpoint, error) {
	ref, err := refFrom(inst.Name)
	if err != nil {
		return Endpoint{}, err
	}
	region, _, _, _ := splitRef(ref)
	port := defaultPort
	if inst.ConnectionPoolConfig != nil && inst.ConnectionPoolConfig.Enabled {
		port = poolerPort
	}

	if inst.PscInstanceConfig != nil {
		for _, ac := range inst.PscInstanceConfig.PscAutoConnections {
			if ac.IpAddress == "" || ac.ConsumerNetworkStatus != "VALID" {
				continue
			}
			np, net, parseErr := parseNetwork(ac.ConsumerNetwork)
			if parseErr != nil {
				return Endpoint{}, fmt.Errorf("parse psc consumer network %q: %w", ac.ConsumerNetwork, parseErr)
			}
			return Endpoint{IP: ac.IpAddress, Port: port, Region: region, NetworkProject: np, Network: net}, nil
		}
	}

	if inst.IpAddress != "" {
		clusterNetwork := ""
		if cluster != nil && cluster.NetworkConfig != nil {
			clusterNetwork = cluster.NetworkConfig.Network
		}
		np, net, parseErr := parseNetwork(clusterNetwork)
		if parseErr != nil {
			return Endpoint{}, fmt.Errorf("parse cluster network %q: %w", clusterNetwork, parseErr)
		}
		return Endpoint{IP: inst.IpAddress, Port: port, Region: region, NetworkProject: np, Network: net}, nil
	}

	return Endpoint{}, fmt.Errorf(
		"AlloyDB instance %s has no private endpoint: no private IP and no active Private Service Connect connection\n"+
			"Fix: give the cluster a private IP (private services access), or accept a PSC endpoint for the instance in your VPC", ref)
}

func parseNetwork(path string) (project, network string, err error) {
	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[0] != "projects" || parts[2] != "global" || parts[3] != "networks" || parts[1] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("unexpected network path format (want projects/P/global/networks/N)")
	}
	return parts[1], parts[4], nil
}

func projectID(ctx context.Context, project string) (string, error) {
	if _, err := strconv.ParseInt(project, 10, 64); err != nil {
		return project, nil //nolint:nilerr // a project that is not a number is already an ID
	}
	svc, err := crmv3.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return "", gcperr.Wrap("get project "+project, err)
	}
	p, err := gcperr.RetryDo(ctx, svc.Projects.Get("projects/"+project).Context(ctx).Do)
	if err != nil {
		return "", gcperr.WrapForbidden("read the ID of project "+project+", the AlloyDB cluster's network project", err,
			"ask for resourcemanager.projects.get on project "+project+" (roles/browser)")
	}
	return p.ProjectId, nil
}

func ListCmd(project string) string {
	return "gcloud alloydb clusters list --project " + project + " --region -"
}

func clusterRef(ref string) string {
	if region, cluster, _, ok := splitRef(ref); ok {
		return region + "/" + cluster
	}
	return ref
}
