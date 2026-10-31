package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/bastion"
	"github.com/pilef-co/bqto/internal/tunnel"
	"github.com/pilef-co/bqto/quota"
)

func defaultPort(socks bool, remotePort int) int {
	if socks {
		return 1080
	}
	return remotePort + 10000
}

func expandSubnet(subnet, networkProject, region string) string {
	if strings.HasPrefix(subnet, "projects/") {
		return subnet
	}
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", networkProject, region, subnet)
}

func parseNetworkRef(ref, defaultProject string) (project, network string, err error) {
	if !strings.Contains(ref, "/") {
		return defaultProject, ref, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 5 || parts[0] != "projects" || parts[2] != "global" || parts[3] != "networks" || parts[1] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("--network %q is not a network\nFix: use a name or projects/PROJECT/global/networks/NAME", ref)
	}
	return parts[1], parts[4], nil
}

func networkFor(flagNetwork, project, serviceProject, serviceNetwork string) (netProject, network string, err error) {
	if flagNetwork != "" {
		return parseNetworkRef(flagNetwork, project)
	}
	return cmp.Or(serviceProject, project), serviceNetwork, nil
}

func pickNetwork(svcNetProj, svcNet, flagNetwork, project string, networks []string) (netProject, network string, err error) {
	if svcNet != "" {
		if svcNetProj == "" {
			svcNetProj = project
		}
		return svcNetProj, svcNet, nil
	}
	if flagNetwork != "" {
		return parseNetworkRef(flagNetwork, project)
	}
	if len(networks) == 1 {
		return project, networks[0], nil
	}
	if len(networks) == 0 {
		return "", "", fmt.Errorf("no VPC networks found in project %q\nFix: pass --network", project)
	}
	return "", "", fmt.Errorf("multiple VPC networks in project %q (%s)\nFix: pass --network with one of them",
		project, strings.Join(networks, ", "))
}

type target struct {
	service      string
	instanceRef  string
	netProject   string
	network      string
	region       string
	svcSubnet    string
	account      string
	impersonated string
	options      map[string]string
	fwd          tunnel.Forward
	spec         ext.Target
	started      ext.Started
}

func (t target) ready(index, localPort int, d sessionDeps) ext.Ready {
	return ext.Ready{
		Index:        index,
		LocalPort:    localPort,
		Account:      t.account,
		Impersonated: t.impersonated,
		Options:      t.options,
		Started:      t.started,
		FreePort:     tunnel.FreePort,
		ReservePort:  d.reserve,
	}
}

func coreHost(cfg connectConfig) ext.Host {
	return ext.Host{
		Progress:    func(msg string) { fmt.Fprintln(os.Stderr, msg) },
		Warn:        func(msg string) { fmt.Fprintln(os.Stderr, "warn:", msg) },
		Interactive: !cfg.NoInput && isTerminal(os.Stdin),
		Pick: func(ctx context.Context, title string, options []string) (string, error) {
			return pickOne(ctx, title, huh.NewOptions(options...))
		},
		SubnetNetwork: bastion.SubnetNetwork,
		Networks:      bastion.Networks,
		PickNetwork:   pickNetwork,
		NetworkFor:    networkFor,
	}
}

func serviceNames(match func(ext.Service) bool) string {
	var names []string
	for _, s := range ext.Services() {
		if match(s) {
			names = append(names, s.Name)
		}
	}
	return joinOr(names)
}

func joinOr(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

func knownService(name string) error {
	if _, ok := ext.LookupService(name); ok {
		return nil
	}
	return unsupportedService(name)
}

func unsupportedService(name string) error {
	if label, pro := ext.ProService(name); pro {
		return ext.ProError{What: label, Drop: "use another --service"}
	}
	return fmt.Errorf("--service %q is not supported\nFix: use %s", name, serviceNames(func(ext.Service) bool { return true }))
}

func resolveVariant(ctx context.Context, h ext.Host, req ext.Request, spec ext.Target, err error) (ext.Target, error) {
	nf, needs := errors.AsType[ext.NeedsFeature](err)
	if !needs {
		return spec, err
	}
	v, found := ext.LookupVariant(nf.Feature)
	if !found {
		label, _ := ext.ProVariant(nf.Feature)
		return ext.Target{}, usage(ext.ProError{What: fmt.Sprintf("%s (%s)", label, nf.Ref), Drop: "connect to an instance instead"})
	}
	variantReq := req
	variantReq.Resolved = nf.Name
	return v.Resolve(ctx, h, variantReq)
}

func resolveTarget(ctx context.Context, cfg connectConfig, explicit map[string]bool) (target, error) {
	ctx = quota.WithProject(ctx, cfg.Project)
	h := coreHost(cfg)
	for _, hook := range ext.Connect() {
		if hook.Resolve == nil {
			continue
		}
		spec, handled, err := hook.Resolve(ctx, h, toExt(cfg))
		if err != nil {
			return target{}, err
		}
		if handled {
			return newTarget(cfg.Service, spec, cfg.options()), nil
		}
	}
	svc, ok := ext.LookupService(cfg.Service)
	if !ok {
		return target{}, unsupportedService(cfg.Service)
	}
	req := ext.Request{
		Project:     cfg.Project,
		Instance:    cfg.Instance,
		Network:     cfg.Network,
		Region:      cfg.Region,
		Interactive: h.Interactive,
		Explicit:    explicit,
		Options:     cfg.options(),
	}
	spec, err := svc.Resolve(ctx, h, req)
	spec, err = resolveVariant(ctx, h, req, spec, err)
	if err != nil {
		return target{}, err
	}
	return newTarget(cfg.Service, spec, req.Options), nil
}

func newTarget(service string, spec ext.Target, options map[string]string) target {
	t := target{
		service:     service,
		instanceRef: spec.Instance,
		netProject:  spec.NetworkProject,
		network:     spec.Network,
		region:      spec.Region,
		svcSubnet:   spec.Subnet,
		options:     options,
		spec:        spec,
	}
	if spec.Forward != nil {
		t.fwd.RemoteHost, t.fwd.RemotePort = spec.Forward.Host, spec.Forward.Port
	}
	return t
}

func sessionNetwork(targets []target) (netProject, network, region string, warnings []string, err error) {
	first := targets[0]
	for _, t := range targets[1:] {
		if t.netProject != first.netProject || t.network != first.network {
			return "", "", "", nil, fmt.Errorf("%s %s and %s %s are on different VPCs (%s/%s, %s/%s)\nFix: open them in separate sessions, or build the stack only from services on one VPC",
				first.service, first.instanceRef, t.service, t.instanceRef,
				first.netProject, first.network, t.netProject, t.network)
		}
		if t.region != first.region {
			warnings = append(warnings, fmt.Sprintf("%s %s is in %s and the bastion will be in %s; it still works inside one VPC, with more latency",
				t.service, t.instanceRef, t.region, first.region))
		}
	}
	return first.netProject, first.network, first.region, warnings, nil
}

func wantedPorts(cfgs []connectConfig, targets []target) []int {
	ports := make([]int, len(targets))
	for i := range targets {
		ports[i] = cfgs[i].Port
		if ports[i] == 0 {
			ports[i] = defaultPort(targets[i].fwd.RemoteHost == "", targets[i].fwd.RemotePort)
		}
	}
	return ports
}

func fixedPorts(cfgs []connectConfig) []bool {
	fixed := make([]bool, len(cfgs))
	for i, c := range cfgs {
		fixed[i] = c.Port != 0
	}
	return fixed
}

func holdPorts(want []int, fixed []bool, listen func(int) (net.Listener, error)) ([]net.Listener, []int, error) {
	held := make([]net.Listener, len(want))
	ports := make([]int, len(want))
	release := func() {
		for _, ln := range held {
			if ln != nil {
				_ = ln.Close()
			}
		}
	}
	for i, w := range want {
		if !fixed[i] {
			continue
		}
		ln, err := listen(w)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("local port %d is taken\nFix: %s", w, tunnel.PortTakenFix())
		}
		held[i], ports[i] = ln, w
	}
	for i, w := range want {
		if fixed[i] {
			continue
		}
		for p := w; p < w+tunnel.PortSearchRange && held[i] == nil; p++ {
			if ln, err := listen(p); err == nil {
				held[i], ports[i] = ln, p
			}
		}
		if held[i] == nil {
			release()
			return nil, nil, fmt.Errorf("find free port: no free local port from %d to %d\nFix: %s", w, w+tunnel.PortSearchRange-1, tunnel.PortRangeTakenFix())
		}
	}
	return held, ports, nil
}
