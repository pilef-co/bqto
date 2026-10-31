package valkey

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	"cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/pilef-co/bqto/quota"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
)

var clientOptions []option.ClientOption

type Endpoint struct {
	IP             string
	Port           int
	Region         string
	NetworkProject string
	Network        string
	TLS            bool
	IAMAuth        bool
}

func List(ctx context.Context, project string) ([]string, error) {
	return listRefs(ctx, project, false)
}

func listRefs(ctx context.Context, project string, withClusters bool) ([]string, error) {
	refs, clusters, err := listAll(ctx, project)
	if err != nil {
		return nil, err
	}
	if withClusters {
		return refs, nil
	}
	return slices.DeleteFunc(refs, func(r string) bool { return clusters[r] }), nil
}

func listAll(ctx context.Context, project string) ([]string, map[string]bool, error) {
	c, err := memorystore.NewRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, nil, gcperr.Wrap("connect to the Memorystore for Valkey API", err)
	}
	defer c.Close()

	type listing struct {
		refs     []string
		clusters map[string]bool
	}
	got, err := gcperr.Retry(ctx, func() (listing, error) {
		it := c.ListInstances(ctx, &memorystorepb.ListInstancesRequest{
			Parent: fmt.Sprintf("projects/%s/locations/-", project),
		})
		l := listing{clusters: map[string]bool{}}
		for {
			inst, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return l, nil
			}
			if err != nil {
				return listing{}, err
			}
			ref, refErr := refFrom(inst.GetName())
			if refErr != nil {
				continue
			}
			l.refs = append(l.refs, ref)
			if inst.GetMode() == memorystorepb.Instance_CLUSTER {
				l.clusters[ref] = true
			}
		}
	})
	if err != nil {
		return nil, nil, gcperr.Wrap("list Memorystore for Valkey instances in project "+project, err)
	}
	slices.Sort(got.refs)
	return got.refs, got.clusters, nil
}

func Resolve(ctx context.Context, project, ref string) (Endpoint, error) {
	c, err := memorystore.NewRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return Endpoint{}, gcperr.Wrap("connect to the Memorystore for Valkey API", err)
	}
	defer c.Close()

	full, err := resolveRef(ctx, project, ref)
	if err != nil {
		return Endpoint{}, err
	}
	inst, err := gcperr.Retry(ctx, func() (*memorystorepb.Instance, error) {
		return c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: full})
	})
	if err != nil {
		return Endpoint{}, gcperr.Get("Memorystore for Valkey instance", ref, project, ListCmd(project), err)
	}
	ep, err := endpointFrom(inst)
	if err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

func resolveRef(ctx context.Context, project, ref string) (string, error) {
	if region, instance, ok := splitRef(ref); ok {
		return fmt.Sprintf("projects/%s/locations/%s/instances/%s", project, region, instance), nil
	}
	refs, clusters, err := listAll(ctx, project)
	if err != nil {
		return "", err
	}
	matches := matchRefs(refs, ref)
	if !clustersOpen() {
		standalone := slices.DeleteFunc(slices.Clone(matches), func(r string) bool { return clusters[r] })
		if len(standalone) > 0 || len(matches) == 0 {
			matches = standalone
		} else {
			matches = matches[:1]
		}
	}
	switch len(matches) {
	case 1:
		region, instance, _ := splitRef(matches[0])
		return fmt.Sprintf("projects/%s/locations/%s/instances/%s", project, region, instance), nil
	case 0:
		return "", gcperr.Missing("Memorystore for Valkey instance", ref, project, ListCmd(project))
	}
	return "", fmt.Errorf("%q matches several instances (%s)\nFix: use REGION/INSTANCE", ref, strings.Join(matches, ", "))
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

type connection struct {
	ip      string
	port    int
	project string
	network string
	kind    memorystorepb.ConnectionType
}

func endpointFrom(inst *memorystorepb.Instance) (Endpoint, error) {
	ref, err := refFrom(inst.GetName())
	if err != nil {
		return Endpoint{}, err
	}
	region, _, _ := splitRef(ref)
	if inst.GetMode() == memorystorepb.Instance_CLUSTER {
		return Endpoint{}, ext.NeedsFeature{Feature: "valkey-cluster", Ref: ref, Name: inst.GetName()}
	}
	conn, err := primaryConnection(inst)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%s: %w", ref, err)
	}
	network := conn.network
	np, name, parseErr := parseNetwork(network)
	if parseErr != nil {
		return Endpoint{}, fmt.Errorf("parse connection network %q: %w", network, parseErr)
	}
	if np == "" {
		np = conn.project
	}
	return Endpoint{
		IP:             conn.ip,
		Port:           conn.port,
		Region:         region,
		NetworkProject: np,
		Network:        name,
		TLS:            inst.GetTransitEncryptionMode() == memorystorepb.Instance_SERVER_AUTHENTICATION,
		IAMAuth:        inst.GetAuthorizationMode() == memorystorepb.Instance_IAM_AUTH,
	}, nil
}

func connections(inst *memorystorepb.Instance) []connection {
	var conns []connection
	for _, ep := range inst.GetEndpoints() {
		for _, detail := range ep.GetConnections() {
			if psc := detail.GetPscAutoConnection(); psc != nil {
				conns = append(conns, connection{psc.GetIpAddress(), int(psc.GetPort()), psc.GetProjectId(), psc.GetNetwork(), psc.GetConnectionType()})
			}
			if psc := detail.GetPscConnection(); psc != nil {
				conns = append(conns, connection{psc.GetIpAddress(), int(psc.GetPort()), psc.GetProjectId(), psc.GetNetwork(), psc.GetConnectionType()})
			}
		}
	}
	for _, psc := range inst.GetPscAutoConnections() { //nolint:staticcheck // instances created before PscAttachmentDetails existed report only PscAutoConnections
		conns = append(conns, connection{psc.GetIpAddress(), int(psc.GetPort()), psc.GetProjectId(), psc.GetNetwork(), psc.GetConnectionType()})
	}
	return conns
}

func findConnection(inst *memorystorepb.Instance, kind memorystorepb.ConnectionType) (connection, bool) {
	for _, c := range connections(inst) {
		if c.kind == kind && c.ip != "" {
			return c, true
		}
	}
	return connection{}, false
}

func primaryConnection(inst *memorystorepb.Instance) (connection, error) {
	if c, ok := findConnection(inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY); ok {
		return c, nil
	}
	return connection{}, fmt.Errorf("no primary endpoint yet (state %s)\nFix: wait until the instance is ACTIVE and run bqto again", inst.GetState())
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

var lookPath = exec.LookPath

func cli() string {
	for _, name := range []string{"valkey-cli", "redis-cli"} {
		if _, err := lookPath(name); err == nil {
			return name
		}
	}
	return "valkey-cli"
}

func Hint(ep Endpoint, localPort int) string {
	cmd := fmt.Sprintf("try: %s -h 127.0.0.1 -p %d", cli(), localPort)
	if ep.TLS {
		cmd += " --tls --cacert server-ca.pem"
	}
	var notes []string
	if ep.TLS {
		notes = append(notes, "CA: gcloud memorystore instances get-certificate-authority INSTANCE --location "+ep.Region+" > server-ca.pem")
	}
	if ep.IAMAuth {
		notes = append(notes, "IAM auth: put REDISCLI_AUTH=\"$(gcloud auth print-access-token)\" before the command; -a would show the token to other users of this machine")
	}
	if len(notes) == 0 {
		return cmd
	}
	return cmd + "\n" + strings.Join(notes, "\n")
}

func ListCmd(project string) string {
	return "gcloud memorystore instances list --project " + project + " --location -"
}

var clustersOpen = func() bool {
	_, ok := ext.LookupVariant("valkey-cluster")
	return ok
}
