package bastion

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/pilef-co/bqto/quota"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
)

var clientOptions []option.ClientOption

type Spec struct {
	Project        string
	Zone           string
	Name           string
	NetworkProject string
	Network        string
	Subnet         string
	Duration       time.Duration
	Spot           bool
	Tags           []string
	MachineType    string
	Image          string
}

func Name() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "bqto-" + hex.EncodeToString(buf)
}

const DefaultImage = ext.DefaultBastionImage

func Image(ctx context.Context, ref string) (name, selfLink string, err error) {
	project, family, image, err := parseImageRef(cmp.Or(ref, DefaultImage))
	if err != nil {
		return "", "", err
	}
	c, err := compute.NewImagesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return "", "", gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	var img *computepb.Image
	if family != "" {
		img, err = gcperr.Retry(ctx, func() (*computepb.Image, error) {
			return c.GetFromFamily(ctx, &computepb.GetFromFamilyImageRequest{Project: project, Family: family})
		})
		if err != nil {
			return "", "", gcperr.Wrap(fmt.Sprintf("get image family %s/%s", project, family), withoutURL(err))
		}
	} else {
		img, err = gcperr.Retry(ctx, func() (*computepb.Image, error) {
			return c.Get(ctx, &computepb.GetImageRequest{Project: project, Image: image})
		})
		if err != nil {
			return "", "", gcperr.Wrap(fmt.Sprintf("get image %s/%s", project, image), withoutURL(err))
		}
	}
	return img.GetName(), img.GetSelfLink(), nil
}

func CheckImage(ref string) error {
	_, _, _, err := parseImageRef(ref)
	return err
}

func parseImageRef(ref string) (project, family, image string, err error) {
	parts := strings.Split(normalizeNetworkURL(ref), "/")
	if len(parts) >= 4 && parts[0] == "projects" && parts[1] != "" && parts[2] == "global" && parts[3] == "images" {
		switch {
		case len(parts) == 5 && parts[4] != "":
			return parts[1], "", parts[4], nil
		case len(parts) == 6 && parts[4] == "family" && parts[5] != "":
			return parts[1], parts[5], "", nil
		}
	}
	return "", "", "", fmt.Errorf("cannot parse image %q\nFix: use projects/P/global/images/IMAGE or projects/P/global/images/family/FAMILY", ref)
}

func Zones(ctx context.Context, project, region string) ([]string, error) {
	c, err := compute.NewZonesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	zones, err := gcperr.Retry(ctx, func() ([]string, error) {
		it := c.List(ctx, &computepb.ListZonesRequest{Project: project})
		var zones []string
		for {
			z, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return zones, nil
			}
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(z.GetName(), region+"-") && z.GetStatus() == "UP" {
				zones = append(zones, z.GetName())
			}
		}
	})
	if err != nil {
		return nil, gcperr.Wrap("list zones in project "+project, err)
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf("no zone of region %s is up in project %s\nFix: check the region name, or try again later", region, project)
	}
	slices.Sort(zones)
	return zones, nil
}

func Subnet(ctx context.Context, networkProject, network, region string) (string, error) {
	subs, err := listSubnets(ctx, networkProject, network, region)
	if err != nil {
		return "", err
	}
	if len(subs) == 0 {
		return "", fmt.Errorf("network %s/%s has no subnet in region %s that can host a VM (proxy-only, PSC and Private NAT subnets are skipped)\nFix: create a subnet in %s%s",
			networkProject, network, region, region, orPass("subnet", ", or pass --subnet"))
	}
	first := slices.MinFunc(subs, func(a, b *computepb.Subnetwork) int {
		return strings.Compare(a.GetSelfLink(), b.GetSelfLink())
	})
	return first.GetSelfLink(), nil
}

func canHostVM(s *computepb.Subnetwork) bool {
	switch s.GetPurpose() {
	case "", computepb.Subnetwork_PRIVATE.String(), computepb.Subnetwork_PRIVATE_RFC_1918.String():
		return true
	}
	return false
}

func listSubnets(ctx context.Context, networkProject, network, region string) ([]*computepb.Subnetwork, error) {
	c, err := compute.NewSubnetworksRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	networkSuffix := "/networks/" + network
	subs, err := gcperr.Retry(ctx, func() ([]*computepb.Subnetwork, error) {
		it := c.List(ctx, &computepb.ListSubnetworksRequest{
			Project: networkProject,
			Region:  region,
		})
		var subs []*computepb.Subnetwork
		for {
			s, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return subs, nil
			}
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(s.GetNetwork(), networkSuffix) && canHostVM(s) {
				subs = append(subs, s)
			}
		}
	})
	if err != nil {
		return nil, gcperr.Wrap(fmt.Sprintf("list subnets of %s/%s in %s", networkProject, network, region), err)
	}
	return subs, nil
}

func Networks(ctx context.Context, project string) ([]string, error) {
	c, err := compute.NewNetworksRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	names, err := gcperr.Retry(ctx, func() ([]string, error) {
		it := c.List(ctx, &computepb.ListNetworksRequest{Project: project})
		var names []string
		for {
			n, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return names, nil
			}
			if err != nil {
				return nil, err
			}
			names = append(names, n.GetName())
		}
	})
	if err != nil {
		return nil, gcperr.Wrap("list networks in project "+project, err)
	}
	slices.Sort(names)
	return names, nil
}

func SubnetWithPGA(ctx context.Context, networkProject, network, region string) (string, error) {
	allSubs, err := listSubnets(ctx, networkProject, network, region)
	if err != nil {
		return "", err
	}
	var pgaSubs []*computepb.Subnetwork
	for _, s := range allSubs {
		if s.GetPrivateIpGoogleAccess() {
			pgaSubs = append(pgaSubs, s)
		}
	}
	return firstPGASubnet(pgaSubs, allSubs, networkProject, network, region)
}

func firstPGASubnet(pgaSubs, allSubs []*computepb.Subnetwork, networkProject, network, region string) (string, error) {
	if len(allSubs) == 0 {
		return "", fmt.Errorf(
			"network %s/%s has no subnet in region %s that can host a VM (proxy-only, PSC and Private NAT subnets are skipped)\nFix: create a subnet in %s%s",
			networkProject, network, region, region, orPass("subnet", ", or pass --subnet"),
		)
	}
	if len(pgaSubs) == 0 {
		names := make([]string, len(allSubs))
		for i, s := range allSubs {
			names[i] = s.GetName()
		}
		slices.Sort(names)
		var sb strings.Builder
		for _, n := range names {
			fmt.Fprintf(&sb, "\n  gcloud compute networks subnets update %s --project %s --region %s --enable-private-ip-google-access",
				n, networkProject, region)
		}
		return "", fmt.Errorf(
			"network %s/%s has subnets in region %s, but none has Private Google Access, which Cloud Run needs"+
				"\nFix: turn it on for one of them:%s",
			networkProject, network, region, sb.String(),
		)
	}
	s := slices.MinFunc(pgaSubs, func(a, b *computepb.Subnetwork) int {
		return strings.Compare(a.GetName(), b.GetName())
	})
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", networkProject, region, s.GetName()), nil
}

func SubnetNetwork(ctx context.Context, subnet string) (networkProject, network string, err error) {
	if _, _, _, err := parseSubnetPath(subnet); err != nil {
		return "", "", fmt.Errorf("subnet network: Google returned a subnet path bqto cannot read: %q\nFix: this is a bqto bug; report it with this message", subnet)
	}
	s, _, _, _, err := getSubnet(ctx, subnet, "subnet network")
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(normalizeNetworkURL(s.GetNetwork()), "/")
	if len(parts) == 5 && parts[0] == "projects" && parts[2] == "global" && parts[3] == "networks" {
		return parts[1], parts[4], nil
	}
	return "", "", fmt.Errorf("subnet %s has a network URL bqto cannot read: %q\nFix: this is a bqto bug; report it with this message", subnet, s.GetNetwork())
}

func CheckPGA(ctx context.Context, subnet string) error {
	s, proj, region, name, err := getSubnet(ctx, subnet, "check PGA")
	if err != nil {
		return err
	}
	if !s.GetPrivateIpGoogleAccess() {
		return fmt.Errorf(
			"subnet %s does not have Private Google Access, which Cloud Run needs\n"+
				"Fix: gcloud compute networks subnets update %s --project %s --region %s --enable-private-ip-google-access",
			subnet, name, proj, region,
		)
	}
	return nil
}

func getSubnet(ctx context.Context, subnet, op string) (s *computepb.Subnetwork, project, region, name string, err error) {
	project, region, name, err = parseSubnetPath(subnet)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("%s: %w", op, err)
	}
	c, err := compute.NewSubnetworksRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, "", "", "", gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()
	s, err = gcperr.Retry(ctx, func() (*computepb.Subnetwork, error) {
		return c.Get(ctx, &computepb.GetSubnetworkRequest{
			Project:    project,
			Region:     region,
			Subnetwork: name,
		})
	})
	if err != nil {
		return nil, "", "", "", gcperr.Wrap("get subnet "+subnet, err)
	}
	return s, project, region, name, nil
}

func CheckSubnet(subnet string) error {
	if !strings.Contains(normalizeNetworkURL(subnet), "/") {
		return nil
	}
	_, _, _, err := parseSubnetPath(subnet)
	return err
}

func parseSubnetPath(subnet string) (project, region, name string, err error) {
	parts := strings.Split(normalizeNetworkURL(subnet), "/")
	if len(parts) == 6 && parts[0] == "projects" && parts[2] == "regions" && parts[4] == "subnetworks" &&
		parts[1] != "" && parts[3] != "" && parts[5] != "" {
		return parts[1], parts[3], parts[5], nil
	}
	return "", "", "", fmt.Errorf("cannot parse subnet path %q\nFix: use projects/P/regions/R/subnetworks/S, or the subnet's short name", normalizeNetworkURL(subnet))
}

const iapCIDR = "35.235.240.0/20"

func coversCIDR(src string) bool {
	_, iap, err := net.ParseCIDR(iapCIDR)
	if err != nil {
		return false
	}
	_, a, err := net.ParseCIDR(src)
	if err != nil {
		return false
	}
	aOnes, _ := a.Mask.Size()
	iapOnes, _ := iap.Mask.Size()
	return a.Contains(iap.IP) && aOnes <= iapOnes
}

type protoPorts interface {
	GetIPProtocol() string
	GetPorts() []string
}

func coversTCP22(a protoPorts) bool {
	return protoCovers22(a.GetIPProtocol(), a.GetPorts())
}

func protoCovers22(proto string, ports []string) bool {
	switch strings.ToLower(proto) {
	case "all":
		return true
	case "tcp", "6":
		return len(ports) == 0 || slices.ContainsFunc(ports, portCovers22)
	}
	return false
}

func portCovers22(p string) bool {
	if loS, hiS, ok := strings.Cut(p, "-"); ok {
		lo, err1 := strconv.Atoi(loS)
		hi, err2 := strconv.Atoi(hiS)
		return err1 == nil && err2 == nil && lo <= 22 && 22 <= hi
	}
	n, err := strconv.Atoi(p)
	return err == nil && n == 22
}

func normalizeNetworkURL(u string) string {
	if idx := strings.Index(u, "projects/"); idx >= 0 {
		return u[idx:]
	}
	return u
}

type firewallPolicy struct {
	name  string
	kind  string
	rules []*computepb.FirewallPolicyRule
}

type verdict int

const (
	verdictNext verdict = iota
	verdictAllow
	verdictDeny
)

const vpcRulesStep = "VPC"

func CheckFirewall(ctx context.Context, networkProject, network, region string) ([]string, []string, error) {
	nc, err := compute.NewNetworksRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, nil, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer nc.Close()

	var warnings []string
	order := ""
	n, err := gcperr.Retry(ctx, func() (*computepb.Network, error) {
		return nc.Get(ctx, &computepb.GetNetworkRequest{Project: networkProject, Network: network})
	})
	switch {
	case err == nil:
		order = n.GetNetworkFirewallPolicyEnforcementOrder()
	case isForbidden(err):
		warnings = append(warnings, "cannot read the firewall policy enforcement order of the network (needs compute.networks.get); assuming the default, VPC firewall rules first")
	default:
		return nil, nil, gcperr.Wrap(fmt.Sprintf("get network %s/%s", networkProject, network), err)
	}

	rc, err := compute.NewRegionNetworkFirewallPoliciesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, nil, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer rc.Close()

	rresp, err := gcperr.Retry(ctx, func() (*computepb.RegionNetworkFirewallPoliciesGetEffectiveFirewallsResponse, error) {
		return rc.GetEffectiveFirewalls(ctx, &computepb.GetEffectiveFirewallsRegionNetworkFirewallPolicyRequest{
			Project: networkProject,
			Region:  region,
			Network: fmt.Sprintf("projects/%s/global/networks/%s", networkProject, network),
		})
	})
	if err == nil {
		tags, wide, err := evalFirewalls(order, regionalPolicies(rresp.GetFirewallPolicys()), rresp.GetFirewalls(), networkProject, network)
		return tags, append(warnings, wide...), err
	}
	if !isForbidden(err) {
		return nil, nil, gcperr.Wrap(fmt.Sprintf("get the effective firewalls of %s/%s in %s", networkProject, network, region), err)
	}
	warnings = append(warnings, "regional network firewall policies were not checked (needs compute.networks.getRegionEffectiveFirewalls, e.g. roles/compute.viewer)")

	gresp, err := gcperr.Retry(ctx, func() (*computepb.NetworksGetEffectiveFirewallsResponse, error) {
		return nc.GetEffectiveFirewalls(ctx, &computepb.GetEffectiveFirewallsNetworkRequest{
			Project: networkProject,
			Network: network,
		})
	})
	if err != nil {
		return nil, nil, gcperr.Wrap(fmt.Sprintf("get the effective firewalls of %s/%s", networkProject, network), err)
	}
	tags, wide, err := evalFirewalls(order, globalPolicies(gresp.GetFirewallPolicys()), gresp.GetFirewalls(), networkProject, network)
	return tags, append(warnings, wide...), err
}

func isForbidden(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusForbidden
}

func globalPolicies(ps []*computepb.NetworksGetEffectiveFirewallsResponseEffectiveFirewallPolicy) []firewallPolicy {
	out := make([]firewallPolicy, len(ps))
	for i, p := range ps {
		out[i] = firewallPolicy{name: p.GetName(), kind: p.GetType(), rules: p.GetRules()}
	}
	return out
}

func regionalPolicies(ps []*computepb.RegionNetworkFirewallPoliciesGetEffectiveFirewallsResponseEffectiveFirewallPolicy) []firewallPolicy {
	out := make([]firewallPolicy, len(ps))
	for i, p := range ps {
		out[i] = firewallPolicy{name: p.GetName(), kind: p.GetType(), rules: p.GetRules()}
	}
	return out
}

func evalFirewalls(order string, policies []firewallPolicy, firewalls []*computepb.Firewall, networkProject, network string) ([]string, []string, error) {
	networkURL := fmt.Sprintf("projects/%s/global/networks/%s", networkProject, network)
	for _, p := range evaluationSteps(order, policies) {
		if p.kind == vpcRulesStep {
			tags, warnings, v, err := evalVPCFirewalls(firewalls, networkURL)
			if v != verdictNext {
				return tags, warnings, err
			}
			continue
		}
		switch v, r := evalPolicy(p, networkURL); v {
		case verdictAllow:
			m := r.GetMatch()
			return nil, wideIngressWarnings(
				fmt.Sprintf("rule priority %d of firewall policy %q", r.GetPriority(), p.name),
				m.GetSrcIpRanges(), policyOtherSources(m), layer4Ports(m.GetLayer4Configs())), nil
		case verdictDeny:
			return nil, nil, fmt.Errorf(
				"firewall policy %q rule priority %d DENY blocks tcp:22 from %s\nFix: ask your network admin to allow tcp:22 from %s before that rule",
				p.name, r.GetPriority(), iapCIDR, iapCIDR)
		}
	}
	return nil, nil, noIAPAllowError(networkProject, network)
}

func layer4Ports(cs []*computepb.FirewallPolicyRuleMatcherLayer4Config) []protoPorts {
	out := make([]protoPorts, 0, len(cs))
	for _, c := range cs {
		out = append(out, layer4{c})
	}
	return out
}

type layer4 struct {
	*computepb.FirewallPolicyRuleMatcherLayer4Config
}

func (l layer4) GetIPProtocol() string { return l.GetIpProtocol() }

func allowedPorts(as []*computepb.Allowed) []protoPorts {
	out := make([]protoPorts, 0, len(as))
	for _, a := range as {
		out = append(out, a)
	}
	return out
}

func vpcOtherSources(fw *computepb.Firewall) []string {
	return slices.Concat(
		labelled("source tags", fw.GetSourceTags()),
		labelled("source service accounts", fw.GetSourceServiceAccounts()))
}

func policyOtherSources(m *computepb.FirewallPolicyRuleMatcher) []string {
	var secureTags []string
	for _, t := range m.GetSrcSecureTags() {
		secureTags = append(secureTags, t.GetName())
	}
	return slices.Concat(
		labelled("secure tags", secureTags),
		labelled("address groups", m.GetSrcAddressGroups()),
		labelled("FQDNs", m.GetSrcFqdns()),
		labelled("regions", m.GetSrcRegionCodes()),
		labelled("threat intelligence lists", m.GetSrcThreatIntelligences()))
}

func labelled(label string, values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return []string{label + " " + strings.Join(values, ", ")}
}

func wideIngressWarnings(rule string, sources, otherSources []string, ports []protoPorts) []string {
	var warnings []string
	var wide []string
	for _, s := range sources {
		if !insideIAP(s) {
			wide = append(wide, s)
		}
	}
	wide = append(wide, otherSources...)
	if len(wide) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s lets the tunnel in, and also allows tcp:22 from %s; %s is all bqto needs",
			rule, strings.Join(wide, ", "), iapCIDR))
	}
	if slices.ContainsFunc(ports, moreThanTCP22) {
		warnings = append(warnings, fmt.Sprintf(
			"%s opens more than tcp:22 from %s into the bastion", rule, iapCIDR))
	}
	return warnings
}

func moreThanTCP22(a protoPorts) bool {
	switch strings.ToLower(a.GetIPProtocol()) {
	case "all":
		return true
	case "tcp", "6":
		return len(a.GetPorts()) == 0 || slices.ContainsFunc(a.GetPorts(), notJust22)
	}
	return false
}

func notJust22(p string) bool {
	loS, hiS, isRange := strings.Cut(p, "-")
	if !isRange {
		hiS = loS
	}
	lo, err1 := strconv.Atoi(loS)
	hi, err2 := strconv.Atoi(hiS)
	return err1 != nil || err2 != nil || lo != 22 || hi != 22
}

var iapPrefix = netip.MustParsePrefix(iapCIDR)

func insideIAP(src string) bool {
	p, err := netip.ParsePrefix(src)
	if err != nil {
		a, addrErr := netip.ParseAddr(src)
		if addrErr != nil {
			return false
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	return p.Addr().Is4() && p.Bits() >= iapPrefix.Bits() && iapPrefix.Contains(p.Masked().Addr())
}

func evaluationSteps(order string, policies []firewallPolicy) []firewallPolicy {
	var hierarchy, system, global, regional []firewallPolicy
	for _, p := range policies {
		switch p.kind {
		case "HIERARCHY":
			hierarchy = append(hierarchy, p)
		case "SYSTEM", "SYSTEM_GLOBAL", "SYSTEM_REGIONAL":
			system = append(system, p)
		case "NETWORK_REGIONAL":
			regional = append(regional, p)
		default:
			global = append(global, p)
		}
	}
	vpc := []firewallPolicy{{kind: vpcRulesStep}}
	if order == "BEFORE_CLASSIC_FIREWALL" {
		return slices.Concat(hierarchy, system, global, regional, vpc)
	}
	return slices.Concat(hierarchy, system, vpc, global, regional)
}

func evalPolicy(p firewallPolicy, networkURL string) (verdict, *computepb.FirewallPolicyRule) {
	sorted := slices.SortedFunc(slices.Values(p.rules), func(a, b *computepb.FirewallPolicyRule) int {
		return cmp.Compare(a.GetPriority(), b.GetPriority())
	})
	for _, r := range sorted {
		if !policyRuleAppliesToIAP(r, networkURL) {
			continue
		}
		switch strings.ToLower(r.GetAction()) {
		case "allow", "apply_security_profile_group":
			return verdictAllow, r
		case "deny":
			return verdictDeny, r
		case "goto_next":
			return verdictNext, r
		}
	}
	return verdictNext, nil
}

func policyRuleAppliesToIAP(r *computepb.FirewallPolicyRule, networkURL string) bool {
	if r.GetDisabled() || r.GetDirection() != "INGRESS" {
		return false
	}
	if len(r.TargetServiceAccounts) > 0 || len(r.TargetSecureTags) > 0 {
		return false
	}
	if len(r.TargetResources) > 0 && !slices.ContainsFunc(r.TargetResources, func(u string) bool {
		return normalizeNetworkURL(u) == networkURL
	}) {
		return false
	}
	m := r.GetMatch()
	if m == nil {
		return false
	}
	return slices.ContainsFunc(m.SrcIpRanges, coversCIDR) &&
		slices.ContainsFunc(m.Layer4Configs, layer4CoversTCP22)
}

func layer4CoversTCP22(c *computepb.FirewallPolicyRuleMatcherLayer4Config) bool {
	return protoCovers22(c.GetIpProtocol(), c.GetPorts())
}

func evalVPCFirewalls(firewalls []*computepb.Firewall, networkURL string) ([]string, []string, verdict, error) {
	var allows []*computepb.Firewall
	for _, fw := range firewalls {
		if ruleMatches(fw, networkURL) {
			allows = append(allows, fw)
		}
	}
	slices.SortStableFunc(allows, func(a, b *computepb.Firewall) int {
		return cmp.Or(
			cmp.Compare(len(vpcWideWarnings(a)), len(vpcWideWarnings(b))),
			cmp.Compare(a.GetPriority(), b.GetPriority()))
	})
	for _, allow := range allows {
		if blockingDeny(firewalls, networkURL, allow) == nil {
			return allow.TargetTags, vpcWideWarnings(allow), verdictAllow, nil
		}
	}

	var allow *computepb.Firewall
	if len(allows) > 0 {
		allow = slices.MinFunc(allows, func(a, b *computepb.Firewall) int {
			return cmp.Compare(a.GetPriority(), b.GetPriority())
		})
	}
	deny := blockingDeny(firewalls, networkURL, allow)
	switch {
	case deny != nil && allow != nil:
		return nil, nil, verdictDeny, fmt.Errorf(
			"INGRESS DENY rule %q (priority %d) blocks tcp:22 from %s before ALLOW rule %q (priority %d)\nFix: ask your network admin to give %q a lower priority number than %q",
			deny.GetName(), deny.GetPriority(), iapCIDR, allow.GetName(), allow.GetPriority(), allow.GetName(), deny.GetName())
	case deny != nil:
		return nil, nil, verdictDeny, fmt.Errorf(
			"INGRESS DENY rule %q (priority %d) blocks tcp:22 from %s\nFix: ask your network admin to allow tcp:22 from %s with a lower priority number",
			deny.GetName(), deny.GetPriority(), iapCIDR, iapCIDR)
	}
	return nil, nil, verdictNext, nil
}

func blockingDeny(firewalls []*computepb.Firewall, networkURL string, allow *computepb.Firewall) *computepb.Firewall {
	var deny *computepb.Firewall
	for _, fw := range firewalls {
		if !denyRuleMatches(fw, networkURL) || !targetsBastion(fw.TargetTags, allow.GetTargetTags()) {
			continue
		}
		if deny == nil || fw.GetPriority() < deny.GetPriority() {
			deny = fw
		}
	}
	if deny == nil || (allow != nil && deny.GetPriority() > allow.GetPriority()) {
		return nil
	}
	return deny
}

func vpcWideWarnings(fw *computepb.Firewall) []string {
	return wideIngressWarnings(
		fmt.Sprintf("VPC firewall rule %q", fw.GetName()),
		fw.GetSourceRanges(), vpcOtherSources(fw), allowedPorts(fw.GetAllowed()))
}

func targetsBastion(ruleTags, bastionTags []string) bool {
	return len(ruleTags) == 0 || slices.ContainsFunc(ruleTags, func(t string) bool {
		return slices.Contains(bastionTags, t)
	})
}

func noIAPAllowError(networkProject, network string) error {
	return fmt.Errorf(
		"no firewall rule or policy allows INGRESS tcp:22 from "+
			"35.235.240.0/20 into network %s/%s, so IAP cannot reach the bastion\n"+
			"(Listing effective firewalls requires compute.networks.getEffectiveFirewalls, "+
			"e.g. roles/compute.networkViewer.)\n"+
			"Fix: ask your network admin for one of:\n"+
			"  • A VPC firewall rule:\n"+
			"      gcloud compute firewall-rules create allow-iap-ssh \\\n"+
			"        --project=%s --network=%s \\\n"+
			"        --direction=INGRESS --action=ALLOW --rules=tcp:22 \\\n"+
			"        --source-ranges=35.235.240.0/20\n"+
			"  • OR a hierarchical/network firewall policy rule allowing INGRESS tcp:22 from 35.235.240.0/20",
		networkProject, network, networkProject, network,
	)
}

func appliesToIAP(fw *computepb.Firewall, networkURL string) bool {
	if normalizeNetworkURL(fw.GetNetwork()) != networkURL || fw.GetDisabled() {
		return false
	}
	if dir := fw.GetDirection(); dir != "" && dir != "INGRESS" {
		return false
	}
	if len(fw.TargetServiceAccounts) > 0 {
		return false
	}
	return slices.ContainsFunc(fw.SourceRanges, coversCIDR)
}

func ruleMatches(fw *computepb.Firewall, networkURL string) bool {
	return appliesToIAP(fw, networkURL) && slices.ContainsFunc(fw.Allowed, func(a *computepb.Allowed) bool { return coversTCP22(a) })
}

func denyRuleMatches(fw *computepb.Firewall, networkURL string) bool {
	return appliesToIAP(fw, networkURL) && slices.ContainsFunc(fw.Denied, func(d *computepb.Denied) bool { return coversTCP22(d) })
}

func Create(ctx context.Context, s Spec) error {
	c, err := compute.NewInstancesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	ext.Debugf("bastion: create %s in %s (machine type %s, image %s, subnet %s, spot %v)", s.Name, s.Zone, cmp.Or(s.MachineType, "e2-micro"), s.Image, s.Subnet, s.Spot)
	op, err := c.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          s.Project,
		Zone:             s.Zone,
		InstanceResource: instanceResource(s),
	})
	if err != nil {
		return createError(s, "insert VM", err)
	}
	start := time.Now()
	if err := op.Wait(ctx); err != nil {
		ext.Debugf("bastion: %s failed after %s: %s", s.Name, time.Since(start).Round(time.Millisecond), gcperr.Message(err))
		return createError(s, "wait for VM", err)
	}
	ext.Debugf("bastion: %s is running after %s", s.Name, time.Since(start).Round(time.Millisecond))
	return nil
}

func createError(s Spec, step string, err error) error {
	err = withoutURL(err)
	if pe := policyViolation(s, err); pe != nil {
		return pe
	}
	op := fmt.Sprintf("create bastion %s in %s (%s)", s.Name, s.Zone, step)
	if strings.Contains(err.Error(), "Invalid value for field 'resource.machineType'") {
		if s.MachineType == "" {
			region := s.Zone[:max(strings.LastIndex(s.Zone, "-"), 0)]
			return &gcperr.Error{Op: op, Err: err, Fix: fmt.Sprintf(`pass --zone with a zone of %s that offers e2-micro; gcloud compute machine-types list --filter="name=e2-micro AND zone~^%s-" lists them%s`, region, region, orPass("machine-type", ", or pass --machine-type with another type"))}
		}
		return &gcperr.Error{Op: op, Err: err, Fix: fmt.Sprintf("gcloud compute machine-types list --zones=%s lists the types, then pass one with --machine-type", s.Zone)}
	}
	return gcperr.Wrap(op, err)
}

var (
	constraintPattern = regexp.MustCompile(`(?:customConstraints|constraints)/[A-Za-z0-9_.]*[A-Za-z0-9_]`)
	vpcscIDPattern    = regexp.MustCompile(`vpcServiceControlsUniqueIdentifier\W+([A-Za-z0-9_-]+)`)
)

type policyError struct {
	msg string
	err error
}

func (e *policyError) Error() string { return e.msg }

func (e *policyError) Unwrap() error { return e.err }

func policyViolation(s Spec, err error) error {
	if _, ok := errors.AsType[*googleapi.Error](err); !ok {
		return nil
	}
	text := err.Error()
	if strings.Contains(text, "Request is prohibited by organization's policy") {
		msg := "VPC Service Controls blocks the Compute Engine API from this machine"
		if m := vpcscIDPattern.FindStringSubmatch(text); m != nil {
			msg += " (vpcServiceControlsUniqueIdentifier: " + m[1] + ")"
		}
		msg += "\nFix: ask an admin for an ingress rule that lets your identity call compute.googleapis.com and iaptunnel.googleapis.com"
		return &policyError{msg: msg, err: err}
	}
	var names []string
	for _, name := range constraintPattern.FindAllString(text, -1) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	var hints []string
	for _, name := range names {
		hint := policyHint(s, name)
		if hint == "" {
			hint = fmt.Sprintf("ask an admin for an exception to %s for project %s", name, s.Project)
		}
		hints = append(hints, hint)
	}
	fix := strings.Join(hints, "\n  and ")
	return &policyError{msg: fmt.Sprintf("organization policy %s blocks the bastion\nFix: %s", strings.Join(names, ", "), fix), err: err}
}

func policyHint(s Spec, constraint string) string {
	switch constraint {
	case "constraints/compute.trustedImageProjects":
		project, _, _, err := parseImageRef(cmp.Or(s.Image, DefaultImage))
		if err != nil {
			return ""
		}
		return fmt.Sprintf("ask an admin to allow projects/%s in %s for project %s", project, constraint, s.Project)
	case "constraints/gcp.resourceLocations":
		return fmt.Sprintf("pass --zone with an allowed zone, or ask an admin to allow %s in %s", s.Zone, constraint)
	case "constraints/compute.restrictSharedVpcSubnetworks":
		if !proFlag("subnet") {
			return fmt.Sprintf("ask an admin to allow %s in %s", s.Subnet, constraint)
		}
		return fmt.Sprintf("pass --subnet with an allowed subnet, or ask an admin to allow %s in %s", s.Subnet, constraint)
	case "constraints/compute.restrictNonConfidentialComputing":
		return fmt.Sprintf("ask an admin to exempt project %s from %s, because bqto cannot create Confidential VMs", s.Project, constraint)
	case "constraints/compute.managed.disallowGlobalDns":
		return fmt.Sprintf("ask an admin to exempt project %s from %s, because bqto does not set zonal DNS on the bastion", s.Project, constraint)
	case "constraints/gcp.restrictNonCmekServices":
		return fmt.Sprintf("ask an admin to exempt project %s from %s, because bqto cannot encrypt the bastion's disk with a customer key", s.Project, constraint)
	}
	return ""
}

func CreateInZones(ctx context.Context, s Spec, zones []string, onRetry func(zone, next string)) (string, error) {
	return tryZones(zones, func(zone string) error {
		s.Zone = zone
		return Create(ctx, s)
	}, onRetry)
}

func tryZones(zones []string, create func(zone string) error, onRetry func(zone, next string)) (string, error) {
	if len(zones) == 0 {
		return "", errors.New("no zones to try")
	}
	var err error
	for i, zone := range zones {
		err = create(zone)
		if !isCapacityError(err) {
			return zone, err
		}
		if i+1 < len(zones) {
			ext.Debugf("bastion: zone %s refused it (%s); trying %s", zone, gcperr.Message(err), zones[i+1])
			onRetry(zone, zones[i+1])
		}
	}
	return zones[len(zones)-1], &noCapacityError{zones: zones, err: err}
}

var capacityCodes = []string{"ZONE_RESOURCE_POOL_EXHAUSTED", "ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS"}

func isCapacityError(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	if slices.ContainsFunc(ge.Errors, func(e googleapi.ErrorItem) bool { return slices.Contains(capacityCodes, e.Reason) }) {
		return true
	}
	return slices.ContainsFunc(capacityCodes, func(code string) bool { return strings.Contains(ge.Message, code) })
}

type noCapacityError struct {
	zones []string
	err   error
}

func (e *noCapacityError) Error() string {
	if len(e.zones) == 1 {
		return fmt.Sprintf("zone %s is out of capacity\nFix: try again later, or pass --zone with another zone in the region", e.zones[0])
	}
	return fmt.Sprintf("zones %s are out of capacity\nFix: try again later%s", strings.Join(e.zones, ", "), orPass("machine-type", ", or pass --machine-type with another machine type"))
}

var proFlag = ext.FlagRegistered

func orPass(flag, alternative string) string {
	if proFlag(flag) {
		return alternative
	}
	return ""
}

func (e *noCapacityError) Unwrap() error { return e.err }

func withoutURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

func MayExistAfter(createErr error) bool {
	var ge *googleapi.Error
	return createErr != nil && !errors.As(createErr, &ge)
}

func instanceResource(s Spec) *computepb.Instance {
	secs := int64(s.Duration.Seconds())
	deleteAction := computepb.Scheduling_DELETE.String()

	scheduling := &computepb.Scheduling{
		MaxRunDuration:            &computepb.Duration{Seconds: &secs},
		InstanceTerminationAction: &deleteAction,
	}
	if s.Spot {
		spot := computepb.Scheduling_SPOT.String()
		scheduling.ProvisioningModel = &spot
	}

	mt := s.MachineType
	if mt == "" {
		mt = "e2-micro"
	}
	machineType := fmt.Sprintf("zones/%s/machineTypes/%s", s.Zone, mt)
	osloginKey := "enable-oslogin"
	osloginVal := "TRUE"
	certKey := "enable-oslogin-certificates"
	bqtoLabel := "session"

	boot := &computepb.AttachedDisk{
		Boot:       new(true),
		AutoDelete: new(true),
		InitializeParams: &computepb.AttachedDiskInitializeParams{
			SourceImage: &s.Image,
		},
	}

	metadata := []*computepb.Items{{Key: &osloginKey, Value: &osloginVal}, {Key: &certKey, Value: &osloginVal}}

	return &computepb.Instance{
		Name:        &s.Name,
		MachineType: &machineType,
		Disks:       []*computepb.AttachedDisk{boot},
		NetworkInterfaces: []*computepb.NetworkInterface{
			{
				Subnetwork: &s.Subnet,
			},
		},
		Metadata: &computepb.Metadata{Items: metadata},
		Labels:   map[string]string{"bqto": bqtoLabel},
		Tags: &computepb.Tags{
			Items: s.Tags,
		},
		Scheduling: scheduling,
		ShieldedInstanceConfig: &computepb.ShieldedInstanceConfig{
			EnableSecureBoot:          new(true),
			EnableVtpm:                new(true),
			EnableIntegrityMonitoring: new(true),
		},
	}
}

func isResourceNotReady(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	for _, e := range ge.Errors {
		if e.Reason == "resourceNotReady" {
			return true
		}
	}
	return false
}

func ID(ctx context.Context, project, zone, name string) (uint64, error) {
	c, err := compute.NewInstancesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return 0, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()
	inst, err := gcperr.Retry(ctx, func() (*computepb.Instance, error) {
		return c.Get(ctx, &computepb.GetInstanceRequest{Project: project, Zone: zone, Instance: name})
	})
	if err != nil {
		return 0, gcperr.Wrap("get bastion "+name, withoutURL(err))
	}
	return inst.GetId(), nil
}

func Status(ctx context.Context, project, zone, name string) (string, bool, error) {
	c, err := compute.NewInstancesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return "", false, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	inst, err := gcperr.Retry(ctx, func() (*computepb.Instance, error) {
		return c.Get(ctx, &computepb.GetInstanceRequest{Project: project, Zone: zone, Instance: name})
	})
	if isNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, gcperr.Wrap("get bastion "+name, withoutURL(err))
	}
	return inst.GetStatus(), true, nil
}

func Delete(ctx context.Context, project, zone, name string, mayBeCreating bool) (bool, error) {
	c, err := compute.NewInstancesRESTClient(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return false, gcperr.Wrap("connect to the Compute Engine API", err)
	}
	defer c.Close()

	req := &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: name}
	ext.Debugf("bastion: delete %s in %s", name, zone)
	start := time.Now()
	err = retryWhile(ctx, deleteRetryInterval, func() error {
		_, err := c.Delete(ctx, req)
		return err
	}, func(err error) bool {
		return retryDelete(err, mayBeCreating, time.Since(start))
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, gcperr.Wrap("delete bastion "+name, err)
	}
	return true, nil
}

const notFoundWindow = 20 * time.Second

func retryDelete(err error, mayBeCreating bool, elapsed time.Duration) bool {
	if isResourceNotReady(err) {
		return true
	}
	return mayBeCreating && isNotFound(err) && elapsed < notFoundWindow
}

func isNotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusNotFound
}

const deleteRetryInterval = 5 * time.Second

func retryWhile(ctx context.Context, interval time.Duration, fn func() error, retry func(error) bool) error {
	for {
		err := fn()
		if !retry(err) {
			return err
		}
		ext.Debugf("bastion: retry in %s after: %s", interval, gcperr.Message(err))
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
}
