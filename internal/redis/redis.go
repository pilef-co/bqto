package redis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"

	"github.com/pilef-co/bqto/quota"
	"google.golang.org/api/option"
	redisapi "google.golang.org/api/redis/v1"
)

var clientOptions []option.ClientOption

type Endpoint struct {
	IP             string
	Port           int
	Region         string
	NetworkProject string
	Network        string
	TLS            bool
	Auth           bool
	ReadIP         string
	ReadPort       int
}

func List(ctx context.Context, project string) ([]string, error) {
	svc, err := redisapi.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, gcperr.Wrap("connect to the Memorystore for Redis API", err)
	}
	names, err := listInstanceNames(ctx, svc, project)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(names))
	for _, name := range names {
		if ref, refErr := refFrom(name); refErr == nil {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return refs, nil
}

func listInstanceNames(ctx context.Context, svc *redisapi.Service, project string) ([]string, error) {
	parent := fmt.Sprintf("projects/%s/locations/-", project)
	names, err := gcperr.Retry(ctx, func() ([]string, error) {
		var names []string
		err := svc.Projects.Locations.Instances.List(parent).Pages(ctx, func(resp *redisapi.ListInstancesResponse) error {
			for _, inst := range resp.Instances {
				names = append(names, inst.Name)
			}
			return nil
		})
		return names, err
	})
	if err != nil {
		return nil, gcperr.Wrap("list Memorystore for Redis instances in project "+project, err)
	}
	return names, nil
}

func listNames(ctx context.Context, svc *redisapi.Service, project string) ([]string, error) {
	names, err := listInstanceNames(ctx, svc, project)
	if err != nil {
		return nil, err
	}
	parent := fmt.Sprintf("projects/%s/locations/-", project)
	clusters, err := gcperr.Retry(ctx, func() ([]string, error) {
		var clusters []string
		err := svc.Projects.Locations.Clusters.List(parent).Pages(ctx, func(resp *redisapi.ListClustersResponse) error {
			for _, c := range resp.Clusters {
				clusters = append(clusters, c.Name)
			}
			return nil
		})
		return clusters, err
	})
	if err != nil {
		return nil, gcperr.Wrap("list Memorystore for Redis clusters in project "+project, err)
	}
	return append(names, clusters...), nil
}

func CheckRef(ref string) error {
	if !strings.HasPrefix(ref, "projects/") {
		return nil
	}
	if _, err := shortRef(ref); err != nil {
		if clustersOpen() {
			return fmt.Errorf("cannot parse %q\nFix: use projects/P/locations/R/instances/N, projects/P/locations/R/clusters/N, or REGION/NAME", ref)
		}
		return fmt.Errorf("cannot parse %q\nFix: use projects/P/locations/R/instances/N, or REGION/NAME", ref)
	}
	return nil
}

func shortRef(name string) (string, error) {
	if ref, err := refFrom(name); err == nil {
		return ref, nil
	}
	return clusterRefFrom(name)
}

func Resolve(ctx context.Context, project, ref string) (Endpoint, error) {
	svc, err := redisapi.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return Endpoint{}, gcperr.Wrap("connect to the Memorystore for Redis API", err)
	}
	candidates, err := candidateNames(ctx, svc, project, ref)
	if err != nil {
		return Endpoint{}, err
	}
	for _, name := range candidates {
		ep, getErr := get(ctx, svc, name)
		if getErr == nil {
			return ep, nil
		}
		if _, needsPro := errors.AsType[ext.NeedsFeature](getErr); needsPro {
			return Endpoint{}, getErr
		}
		if !gcperr.NotFound(getErr) {
			return Endpoint{}, gcperr.Wrap("get Memorystore for Redis "+name, getErr)
		}
	}
	return Endpoint{}, gcperr.Missing(kindName(), ref, project, ListCmd(project))
}

var clustersOpen = func() bool {
	_, ok := ext.LookupVariant("redis-cluster")
	return ok
}

func kindName() string {
	if clustersOpen() {
		return "Memorystore for Redis instance or cluster"
	}
	return "Memorystore for Redis instance"
}

func get(ctx context.Context, svc *redisapi.Service, name string) (Endpoint, error) {
	if strings.Contains(name, "/clusters/") {
		if _, err := gcperr.RetryDo(ctx, svc.Projects.Locations.Clusters.Get(name).Context(ctx).Do); err != nil {
			return Endpoint{}, err
		}
		ref, _ := clusterRefFrom(name)
		return Endpoint{}, ext.NeedsFeature{Feature: "redis-cluster", Ref: ref, Name: name}
	}
	inst, err := gcperr.RetryDo(ctx, svc.Projects.Locations.Instances.Get(name).Context(ctx).Do)
	if err != nil {
		return Endpoint{}, err
	}
	return endpointFrom(inst)
}

func candidateNames(ctx context.Context, svc *redisapi.Service, project, ref string) ([]string, error) {
	if strings.HasPrefix(ref, "projects/") {
		if err := CheckRef(ref); err != nil {
			return nil, err
		}
		return []string{ref}, nil
	}
	if region, name, ok := splitRef(ref); ok {
		return []string{
			fmt.Sprintf("projects/%s/locations/%s/instances/%s", project, region, name),
			fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, region, name),
		}, nil
	}
	if !clustersOpen() {
		return freeCandidates(ctx, svc, project, ref)
	}
	names, err := listNames(ctx, svc, project)
	if err != nil {
		return nil, err
	}
	matches := matchRefs(names, ref)
	switch len(matches) {
	case 0:
		return nil, gcperr.Missing("Memorystore for Redis instance or cluster", ref, project, ListCmd(project))
	case 1:
		return matches, nil
	}
	slices.Sort(matches)
	return nil, fmt.Errorf("%q matches several instances or clusters\nFix: pass one of: %s", ref, strings.Join(matches, ", "))
}

func freeCandidates(ctx context.Context, svc *redisapi.Service, project, ref string) ([]string, error) {
	names, err := listInstanceNames(ctx, svc, project)
	if err != nil {
		return nil, err
	}
	matches := matchRefs(names, ref)
	switch len(matches) {
	case 1:
		return matches, nil
	case 0:
		if clusters := clusterMatches(ctx, svc, project, ref); len(clusters) > 0 {
			return clusters[:1], nil
		}
		return nil, gcperr.Missing("Memorystore for Redis instance", ref, project, ListCmd(project))
	}
	slices.Sort(matches)
	return nil, fmt.Errorf("%q matches several instances\nFix: pass one of: %s", ref, strings.Join(matches, ", "))
}

func clusterMatches(ctx context.Context, svc *redisapi.Service, project, ref string) []string {
	parent := fmt.Sprintf("projects/%s/locations/-", project)
	clusters, err := gcperr.Retry(ctx, func() ([]string, error) {
		var clusters []string
		err := svc.Projects.Locations.Clusters.List(parent).Pages(ctx, func(resp *redisapi.ListClustersResponse) error {
			for _, c := range resp.Clusters {
				clusters = append(clusters, c.Name)
			}
			return nil
		})
		return clusters, err
	})
	if err != nil {
		return nil
	}
	return matchRefs(clusters, ref)
}

func clusterRefFrom(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "clusters" {
		return "", fmt.Errorf("unexpected cluster name %q", name)
	}
	return parts[3] + "/" + parts[5], nil
}

func matchRefs(names []string, ref string) []string {
	var matches []string
	for _, n := range names {
		if strings.HasSuffix(n, "/"+ref) {
			matches = append(matches, n)
		}
	}
	return matches
}

func splitRef(ref string) (region, instance string, ok bool) {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || slices.Contains(parts, "") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func refFrom(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "instances" {
		return "", fmt.Errorf("unexpected instance name %q", name)
	}
	return parts[3] + "/" + parts[5], nil
}

func endpointFrom(inst *redisapi.Instance) (Endpoint, error) {
	ref, err := refFrom(inst.Name)
	if err != nil {
		return Endpoint{}, err
	}
	region, _, _ := splitRef(ref)
	if inst.Host == "" {
		return Endpoint{}, fmt.Errorf("instance %s has no endpoint yet (state %s)\nFix: wait until it is READY and run bqto again", ref, inst.State)
	}
	np, net, err := parseNetwork(inst.AuthorizedNetwork)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse authorized network %q: %w", inst.AuthorizedNetwork, err)
	}
	return Endpoint{
		IP:             inst.Host,
		Port:           int(inst.Port),
		Region:         region,
		NetworkProject: np,
		Network:        net,
		TLS:            inst.TransitEncryptionMode == "SERVER_AUTHENTICATION",
		Auth:           inst.AuthEnabled,
		ReadIP:         inst.ReadEndpoint,
		ReadPort:       int(inst.ReadEndpointPort),
	}, nil
}

func parseNetwork(path string) (project, network string, err error) {
	if path == "" {
		return "", "", fmt.Errorf("empty network")
	}
	if !strings.Contains(path, "/") {
		return "", path, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[0] != "projects" || parts[2] != "global" || parts[3] != "networks" || parts[1] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("unexpected network path format (want projects/P/global/networks/N)")
	}
	return parts[1], parts[4], nil
}

func Hint(ep Endpoint, ref string, localPort int) string {
	cmd := fmt.Sprintf("try: redis-cli -h 127.0.0.1 -p %d", localPort)
	if ep.TLS {
		cmd += " --tls --cacert server-ca.pem"
	}
	var notes []string
	if ep.TLS {
		notes = append(notes, fmt.Sprintf("CA: gcloud redis instances describe %s --region %s --format='value(serverCaCerts[0].cert)' > server-ca.pem", instanceOf(ref), ep.Region))
	}
	if ep.Auth {
		notes = append(notes, fmt.Sprintf("AUTH: gcloud redis instances get-auth-string %s --region %s", instanceOf(ref), ep.Region))
	}
	if ep.ReadIP != "" {
		notes = append(notes, fmt.Sprintf("read endpoint %s:%d is not forwarded", ep.ReadIP, ep.ReadPort))
	}
	if len(notes) == 0 {
		return cmd
	}
	return cmd + "\n" + strings.Join(notes, "\n")
}

func instanceOf(ref string) string {
	if _, instance, ok := splitRef(ref); ok {
		return instance
	}
	return ref
}

func ListCmd(project string) string {
	return "gcloud redis instances list --project " + project + " --region -"
}
