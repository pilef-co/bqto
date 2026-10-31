package bastion

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
)

func iapLayer4TCP22() []*computepb.FirewallPolicyRuleMatcherLayer4Config {
	proto := "tcp"
	return []*computepb.FirewallPolicyRuleMatcherLayer4Config{
		{IpProtocol: &proto, Ports: []string{"22"}},
	}
}

func iapMatcher() *computepb.FirewallPolicyRuleMatcher {
	return &computepb.FirewallPolicyRuleMatcher{
		SrcIpRanges:   []string{"35.235.240.0/20"},
		Layer4Configs: iapLayer4TCP22(),
	}
}

func policyRule(priority int32, action, direction string) *computepb.FirewallPolicyRule {
	disabled := false
	return &computepb.FirewallPolicyRule{
		Priority:  &priority,
		Action:    &action,
		Direction: &direction,
		Disabled:  &disabled,
		Match:     iapMatcher(),
	}
}

func vpcAllow(networkProject, network string, priority int32, tags []string) *computepb.Firewall {
	ingress := "INGRESS"
	disabled := false
	proto := "tcp"
	netURL := "projects/" + networkProject + "/global/networks/" + network
	return &computepb.Firewall{
		Network:      &netURL,
		Direction:    &ingress,
		Disabled:     &disabled,
		Priority:     &priority,
		SourceRanges: []string{"35.235.240.0/20"},
		Allowed:      []*computepb.Allowed{{IPProtocol: &proto, Ports: []string{"22"}}},
		TargetTags:   tags,
	}
}

func TestEvalFirewalls(t *testing.T) {
	t.Parallel()
	const proj, net = "p", "net"
	const after, before = "AFTER_CLASSIC_FIREWALL", "BEFORE_CLASSIC_FIREWALL"

	policy := func(kind string, rules ...*computepb.FirewallPolicyRule) firewallPolicy {
		return firewallPolicy{name: kind + "-policy", kind: kind, rules: rules}
	}
	vpcDeny := func(priority int32) *computepb.Firewall {
		fw := vpcAllow(proj, net, priority, nil)
		fw.Name = new("deny-iap")
		fw.Denied = []*computepb.Denied{{IPProtocol: new("tcp"), Ports: []string{"22"}}}
		fw.Allowed = nil
		return fw
	}
	withTags := func(fw *computepb.Firewall, tags ...string) *computepb.Firewall {
		fw.TargetTags = tags
		return fw
	}
	withTargets := func(r *computepb.FirewallPolicyRule, targets ...string) *computepb.FirewallPolicyRule {
		r.TargetResources = targets
		return r
	}

	tests := []struct {
		desc      string
		order     string
		policies  []firewallPolicy
		firewalls []*computepb.Firewall
		wantTags  []string
		wantWarn  []string
		wantErr   string
	}{
		{
			desc:     "hierarchical allow",
			policies: []firewallPolicy{policy("HIERARCHY", policyRule(1000, "allow", "INGRESS"))},
		},
		{
			desc:      "hierarchical deny blocks even if VPC allows",
			policies:  []firewallPolicy{policy("HIERARCHY", policyRule(1000, "deny", "INGRESS"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
			wantErr:   "DENY blocks tcp:22",
		},
		{
			desc:      "goto_next falls through to VPC allow and returns its tags",
			policies:  []firewallPolicy{policy("HIERARCHY", policyRule(1000, "goto_next", "INGRESS"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, []string{"iap-ssh"})},
			wantTags:  []string{"iap-ssh"},
		},
		{
			desc:    "no rules: implied deny",
			wantErr: "35.235.240.0/20",
		},
		{
			desc: "policy rule targeting a service account is skipped",
			policies: []firewallPolicy{policy("HIERARCHY", func() *computepb.FirewallPolicyRule {
				r := policyRule(1000, "allow", "INGRESS")
				r.TargetServiceAccounts = []string{"sa@p.iam.gserviceaccount.com"}
				return r
			}())},
			wantErr: "35.235.240.0/20",
		},
		{
			desc:      "default order: VPC allow wins over a global network policy deny",
			policies:  []firewallPolicy{policy("NETWORK", policyRule(1000, "deny", "INGRESS"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
		},
		{
			desc:      "before classic: global network policy deny wins over VPC allow",
			order:     before,
			policies:  []firewallPolicy{policy("NETWORK", policyRule(1000, "deny", "INGRESS"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
			wantErr:   "NETWORK-policy",
		},
		{
			desc:      "before classic: regional network policy deny wins over VPC allow",
			order:     before,
			policies:  []firewallPolicy{policy("NETWORK_REGIONAL", policyRule(1000, "deny", "INGRESS"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
			wantErr:   "NETWORK_REGIONAL-policy",
		},
		{
			desc:     "after classic: regional network policy deny applies when nothing earlier matches",
			order:    after,
			policies: []firewallPolicy{policy("NETWORK_REGIONAL", policyRule(1000, "deny", "INGRESS"))},
			wantErr:  "NETWORK_REGIONAL-policy",
		},
		{
			desc:     "regional network policy allow is enough",
			policies: []firewallPolicy{policy("NETWORK_REGIONAL", policyRule(1000, "allow", "INGRESS"))},
		},
		{
			desc:     "global network policy is evaluated before the regional one",
			policies: []firewallPolicy{policy("NETWORK_REGIONAL", policyRule(1000, "allow", "INGRESS")), policy("NETWORK", policyRule(1000, "deny", "INGRESS"))},
			wantErr:  "NETWORK-policy",
		},
		{
			desc:      "regional system policy is evaluated before VPC rules",
			policies:  []firewallPolicy{policy("SYSTEM_REGIONAL", policyRule(1000, "deny", "INGRESS"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
			wantErr:   "SYSTEM_REGIONAL-policy",
		},
		{
			desc:      "VPC deny at equal priority wins",
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil), vpcDeny(1000)},
			wantErr:   "deny-iap",
		},
		{
			desc:      "VPC allow at higher priority wins over a deny",
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 900, nil), vpcDeny(1000)},
		},
		{
			desc:      "VPC deny for a tag the bastion does not carry is skipped",
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil), withTags(vpcDeny(900), "db-servers")},
		},
		{
			desc:      "VPC deny for the tag the bastion takes from the allow rule blocks",
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, []string{"iap-ssh"}), withTags(vpcDeny(900), "iap-ssh")},
			wantErr:   "deny-iap",
		},
		{
			desc:      "VPC deny alone blocks",
			firewalls: []*computepb.Firewall{vpcDeny(1000)},
			wantErr:   "deny-iap",
		},
		{
			desc:      "policy rule for another network is skipped",
			policies:  []firewallPolicy{policy("HIERARCHY", withTargets(policyRule(1000, "deny", "INGRESS"), "https://www.googleapis.com/compute/v1/projects/p/global/networks/other"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
		},
		{
			desc:      "policy rule for this network applies",
			policies:  []firewallPolicy{policy("HIERARCHY", withTargets(policyRule(1000, "deny", "INGRESS"), "https://www.googleapis.com/compute/v1/projects/p/global/networks/net"))},
			firewalls: []*computepb.Firewall{vpcAllow(proj, net, 1000, nil)},
			wantErr:   "HIERARCHY-policy",
		},
		{
			desc:     "apply_security_profile_group allows",
			policies: []firewallPolicy{policy("HIERARCHY", policyRule(1000, "apply_security_profile_group", "INGRESS"))},
		},
		{
			desc: "a hierarchical allow from anywhere, on every protocol",
			policies: []firewallPolicy{policy("HIERARCHY", func() *computepb.FirewallPolicyRule {
				r := policyRule(1000, "allow", "INGRESS")
				r.Match.SrcIpRanges = []string{"0.0.0.0/0"}
				r.Match.Layer4Configs = []*computepb.FirewallPolicyRuleMatcherLayer4Config{{IpProtocol: new("all")}}
				return r
			}())},
			wantWarn: []string{"also allows tcp:22 from 0.0.0.0/0", "opens more than tcp:22"},
		},
		{
			desc: "a narrow IAP rule is preferred over a wide one with a better priority",
			firewalls: []*computepb.Firewall{
				func() *computepb.Firewall {
					fw := vpcAllow(proj, net, 900, []string{"ssh"})
					fw.Name = new("allow-ssh-anywhere")
					fw.SourceRanges = []string{"0.0.0.0/0"}
					return fw
				}(),
				vpcAllow(proj, net, 1000, []string{"iap"}),
			},
			wantTags: []string{"iap"},
		},
		{
			desc: "a VPC allow from anywhere",
			firewalls: []*computepb.Firewall{func() *computepb.Firewall {
				fw := vpcAllow(proj, net, 1000, nil)
				fw.Name = new("default-allow-ssh")
				fw.SourceRanges = []string{"0.0.0.0/0"}
				return fw
			}()},
			wantWarn: []string{`VPC firewall rule "default-allow-ssh" lets the tunnel in, and also allows tcp:22 from 0.0.0.0/0`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			tags, warnings, err := evalFirewalls(tc.order, tc.policies, tc.firewalls, proj, net)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !slices.Equal(tags, tc.wantTags) {
					t.Errorf("tags = %v, want %v", tags, tc.wantTags)
				}
				if len(warnings) != len(tc.wantWarn) {
					t.Fatalf("warnings = %q, want %d", warnings, len(tc.wantWarn))
				}
				for i, w := range tc.wantWarn {
					if !strings.Contains(warnings[i], w) {
						t.Errorf("warning %q does not contain %q", warnings[i], w)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCoversCIDR(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src  string
		want bool
	}{
		{"0.0.0.0/0", true},
		{"35.235.240.0/20", true},
		{"35.235.0.0/16", true},
		{"10.0.0.0/8", false},
		{"35.235.240.0/24", false},
	}
	for _, tc := range tests {
		got := coversCIDR(tc.src)
		if got != tc.want {
			t.Errorf("coversCIDR(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestCoversTCP22(t *testing.T) {
	tests := []struct {
		desc  string
		proto string
		ports []string
		want  bool
	}{
		{"exact port", "tcp", []string{"22"}, true},
		{"range covering 22", "tcp", []string{"20-30"}, true},
		{"empty ports = all tcp", "tcp", []string{}, true},
		{"all protocol", "all", []string{}, true},
		{"all protocol with ports", "all", []string{"80"}, true},
		{"udp", "udp", []string{"22"}, false},
		{"icmp", "icmp", []string{}, false},
		{"tcp wrong port", "tcp", []string{"80"}, false},
		{"tcp range not covering 22", "tcp", []string{"23-30"}, false},
	}
	for _, tc := range tests {
		a := &computepb.Allowed{
			IPProtocol: new(tc.proto),
			Ports:      tc.ports,
		}
		got := coversTCP22(a)
		if got != tc.want {
			t.Errorf("coversTCP22 %s: got %v, want %v", tc.desc, got, tc.want)
		}
	}
}

func TestName(t *testing.T) {
	a, b := Name(), Name()
	if !regexp.MustCompile(`^bqto-[0-9a-f]{8}$`).MatchString(a) {
		t.Errorf("Name() = %q, want bqto-<8 hex>", a)
	}
	if a == b {
		t.Errorf("Name() returned the same value twice: %q", a)
	}
}

func TestNormalizeNetworkURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			"https://www.googleapis.com/compute/v1/projects/my-proj/global/networks/my-net",
			"projects/my-proj/global/networks/my-net",
		},
		{
			"projects/my-proj/global/networks/my-net",
			"projects/my-proj/global/networks/my-net",
		},
		{"other-format", "other-format"},
	}
	for _, tc := range tests {
		got := normalizeNetworkURL(tc.input)
		if got != tc.want {
			t.Errorf("normalizeNetworkURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFirstPGASubnet(t *testing.T) {
	sub := func(name string) *computepb.Subnetwork {
		return &computepb.Subnetwork{Name: new(name)}
	}
	tests := []struct {
		desc    string
		pgaSubs []*computepb.Subnetwork
		allSubs []*computepb.Subnetwork
		want    string
		wantErr string
	}{
		{
			desc:    "no subnets in region → error",
			pgaSubs: nil,
			allSubs: nil,
			wantErr: "Private NAT subnets are skipped",
		},
		{
			desc:    "subnets exist but none has PGA → error naming subnets",
			pgaSubs: nil,
			allSubs: []*computepb.Subnetwork{sub("subnet-a"), sub("subnet-b")},
			wantErr: "subnet-a",
		},
		{
			desc:    "single PGA subnet → path returned",
			pgaSubs: []*computepb.Subnetwork{sub("my-subnet")},
			allSubs: []*computepb.Subnetwork{sub("my-subnet")},
			want:    "projects/my-proj/regions/us-central1/subnetworks/my-subnet",
		},
		{
			desc:    "multiple PGA subnets → first by name",
			pgaSubs: []*computepb.Subnetwork{sub("zz-subnet"), sub("aa-subnet"), sub("mm-subnet")},
			allSubs: []*computepb.Subnetwork{sub("zz-subnet"), sub("aa-subnet"), sub("mm-subnet")},
			want:    "projects/my-proj/regions/us-central1/subnetworks/aa-subnet",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := firstPGASubnet(tc.pgaSubs, tc.allSubs, "my-proj", "default", "us-central1")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("firstPGASubnet = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTrustedImageHintNamesImageProject(t *testing.T) {
	spec := Spec{Project: "p", Image: "https://www.googleapis.com/compute/v1/projects/team-images/global/images/bqto-base-v1"}
	msg := "Constraint constraints/compute.trustedImageProjects violated for project p. Use of images from project team-images is prohibited."
	err := policyViolation(spec, &googleapi.Error{Code: 412, Message: msg})
	want := "Fix: ask an admin to allow projects/team-images in constraints/compute.trustedImageProjects for project p"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("policyViolation() = %v, want it to contain %q", err, want)
	}
}

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		input       string
		wantProject string
		wantFamily  string
		wantImage   string
		wantErr     bool
	}{
		{input: DefaultImage, wantProject: "debian-cloud", wantFamily: "debian-13"},
		{input: "projects/team-images/global/images/bqto-base-v1", wantProject: "team-images", wantImage: "bqto-base-v1"},
		{input: "https://www.googleapis.com/compute/v1/projects/p/global/images/family/f", wantProject: "p", wantFamily: "f"},
		{input: "debian-13", wantErr: true},
		{input: "projects/p/global/images/", wantErr: true},
		{input: "projects/p/global/images/family/", wantErr: true},
		{input: "projects/p/global/networks/n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			project, family, image, err := parseImageRef(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got (%q, %q, %q)", project, family, image)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if project != tc.wantProject || family != tc.wantFamily || image != tc.wantImage {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", project, family, image, tc.wantProject, tc.wantFamily, tc.wantImage)
			}
		})
	}
}

func TestParseSubnetPath(t *testing.T) {
	tests := []struct {
		input      string
		wantProj   string
		wantRegion string
		wantName   string
		wantErr    bool
	}{
		{
			input:      "projects/my-proj/regions/us-central1/subnetworks/my-subnet",
			wantProj:   "my-proj",
			wantRegion: "us-central1",
			wantName:   "my-subnet",
		},
		{
			input:      "https://www.googleapis.com/compute/v1/projects/my-proj/regions/us-central1/subnetworks/my-subnet",
			wantProj:   "my-proj",
			wantRegion: "us-central1",
			wantName:   "my-subnet",
		},
		{input: "not-a-path", wantErr: true},
		{input: "projects/p/global/networks/n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			proj, region, name, err := parseSubnetPath(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (proj=%q region=%q name=%q)", proj, region, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if proj != tc.wantProj || region != tc.wantRegion || name != tc.wantName {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					proj, region, name, tc.wantProj, tc.wantRegion, tc.wantName)
			}
		})
	}
}

func TestRuleMatchesNetworkExact(t *testing.T) {
	allow22 := []*computepb.Allowed{{IPProtocol: new("tcp"), Ports: []string{"22"}}}
	iapRanges := []string{"35.235.240.0/20"}
	ingress := "INGRESS"
	disabled := false

	wantURL := "projects/p/global/networks/net"

	tests := []struct {
		desc      string
		ruleNet   string
		wantMatch bool
	}{
		{
			"full URL exact match",
			"https://www.googleapis.com/compute/v1/projects/p/global/networks/net",
			true,
		},
		{
			"short form exact match",
			"projects/p/global/networks/net",
			true,
		},
		{
			"substring: net must not match subnet",
			"projects/p/global/networks/subnet",
			false,
		},
		{
			"substring: net must not match net-2",
			"projects/p/global/networks/net-2",
			false,
		},
		{
			"different project",
			"projects/q/global/networks/net",
			false,
		},
	}
	for _, tc := range tests {
		fw := &computepb.Firewall{
			Network:      new(tc.ruleNet),
			Direction:    &ingress,
			Disabled:     &disabled,
			Allowed:      allow22,
			SourceRanges: iapRanges,
		}
		got := ruleMatches(fw, wantURL)
		if got != tc.wantMatch {
			t.Errorf("ruleMatches %s: got %v, want %v", tc.desc, got, tc.wantMatch)
		}
	}
}

func TestInstanceResource(t *testing.T) {
	base := Spec{
		Project:  "p",
		Zone:     "europe-west1-b",
		Name:     "bqto-0123abcd",
		Subnet:   "projects/np/regions/europe-west1/subnetworks/s",
		Duration: 30 * time.Minute,
		Tags:     []string{"iap-ssh"},
		Image:    "https://www.googleapis.com/compute/v1/projects/debian-cloud/global/images/debian-13-trixie-v20260910",
	}

	tests := []struct {
		desc            string
		spec            func(Spec) Spec
		wantMachineType string
		wantSpot        bool
	}{
		{desc: "defaults", spec: func(s Spec) Spec { return s }, wantMachineType: "zones/europe-west1-b/machineTypes/e2-micro"},
		{desc: "machine type override", spec: func(s Spec) Spec { s.MachineType = "e2-small"; return s }, wantMachineType: "zones/europe-west1-b/machineTypes/e2-small"},
		{desc: "spot", spec: func(s Spec) Spec { s.Spot = true; return s }, wantMachineType: "zones/europe-west1-b/machineTypes/e2-micro", wantSpot: true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			inst := instanceResource(tc.spec(base))

			sh := inst.GetShieldedInstanceConfig()
			if !sh.GetEnableSecureBoot() || !sh.GetEnableVtpm() || !sh.GetEnableIntegrityMonitoring() {
				t.Errorf("shielded config = %+v, want secure boot, vTPM and integrity monitoring on", sh)
			}
			if got := inst.GetDisks()[0].GetInitializeParams().GetSourceImage(); got != base.Image {
				t.Errorf("source image = %q, want %q", got, base.Image)
			}
			if got := inst.GetMachineType(); got != tc.wantMachineType {
				t.Errorf("machine type = %q, want %q", got, tc.wantMachineType)
			}
			if len(inst.GetNetworkInterfaces()) != 1 {
				t.Fatalf("network interfaces = %d, want 1", len(inst.GetNetworkInterfaces()))
			}
			nic := inst.GetNetworkInterfaces()[0]
			if len(nic.GetAccessConfigs()) != 0 {
				t.Errorf("access configs = %v, want none (no external IP)", nic.GetAccessConfigs())
			}
			if nic.GetSubnetwork() != base.Subnet {
				t.Errorf("subnetwork = %q, want %q", nic.GetSubnetwork(), base.Subnet)
			}
			if len(inst.GetServiceAccounts()) != 0 {
				t.Errorf("service accounts = %v, want none", inst.GetServiceAccounts())
			}
			sched := inst.GetScheduling()
			if got := sched.GetInstanceTerminationAction(); got != "DELETE" {
				t.Errorf("termination action = %q, want DELETE", got)
			}
			if got := sched.GetMaxRunDuration().GetSeconds(); got != 1800 {
				t.Errorf("max run duration = %ds, want 1800s", got)
			}
			if gotSpot := sched.GetProvisioningModel() == "SPOT"; gotSpot != tc.wantSpot {
				t.Errorf("provisioning model = %q, want spot=%v", sched.GetProvisioningModel(), tc.wantSpot)
			}
			meta := map[string]string{}
			for _, it := range inst.GetMetadata().GetItems() {
				meta[it.GetKey()] = it.GetValue()
			}
			for _, key := range []string{"enable-oslogin", "enable-oslogin-certificates"} {
				if meta[key] != "TRUE" {
					t.Errorf("%s = %q, want TRUE", key, meta[key])
				}
			}
			if got := inst.GetLabels()["bqto"]; got != "session" {
				t.Errorf("label bqto = %q, want session", got)
			}
			if got := inst.GetTags().GetItems(); len(got) != 1 || got[0] != "iap-ssh" {
				t.Errorf("tags = %v, want [iap-ssh]", got)
			}
		})
	}
}

func TestRetryWhile(t *testing.T) {
	notReady := &googleapi.Error{Code: 400, Errors: []googleapi.ErrorItem{{Reason: "resourceNotReady"}}}
	other := errors.New("boom")

	tests := []struct {
		desc      string
		results   []error
		cancel    bool
		wantCalls int
		wantErr   error
	}{
		{desc: "success first try", results: []error{nil}, wantCalls: 1},
		{desc: "not ready twice then success", results: []error{notReady, notReady, nil}, wantCalls: 3},
		{desc: "other error is not retried", results: []error{other}, wantCalls: 1, wantErr: other},
		{desc: "not ready until ctx done", results: []error{notReady}, cancel: true, wantCalls: 1, wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			calls := 0
			err := retryWhile(ctx, time.Millisecond, func() error {
				r := tc.results[min(calls, len(tc.results)-1)]
				calls++
				return r
			}, isResourceNotReady)
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRetryDelete(t *testing.T) {
	notReady := &googleapi.Error{Code: 400, Errors: []googleapi.ErrorItem{{Reason: "resourceNotReady"}}}
	notFound := &googleapi.Error{Code: 404}
	tests := []struct {
		desc          string
		err           error
		mayBeCreating bool
		elapsed       time.Duration
		want          bool
	}{
		{desc: "not ready", err: notReady, want: true},
		{desc: "not found while the create may still be running", err: notFound, mayBeCreating: true, elapsed: 5 * time.Second, want: true},
		{desc: "not found after the window", err: notFound, mayBeCreating: true, elapsed: 25 * time.Second},
		{desc: "not found when no create is running", err: notFound},
		{desc: "other error", err: errors.New("boom"), mayBeCreating: true},
		{desc: "success", mayBeCreating: true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := retryDelete(tc.err, tc.mayBeCreating, tc.elapsed); got != tc.want {
				t.Errorf("retryDelete() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMayExistAfter(t *testing.T) {
	tests := []struct {
		desc string
		err  error
		want bool
	}{
		{desc: "created", err: nil},
		{desc: "API rejected the create", err: fmt.Errorf("create vm: %w", &googleapi.Error{Code: 412})},
		{desc: "interrupted", err: fmt.Errorf("create vm: %w", context.Canceled), want: true},
		{desc: "network error", err: fmt.Errorf("create vm: %w", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("connection reset")}), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := MayExistAfter(tc.err); got != tc.want {
				t.Errorf("MayExistAfter() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithoutURL(t *testing.T) {
	inner := errors.New("interrupt signal received")
	if got := withoutURL(&url.Error{Op: "Post", URL: "https://compute.googleapis.com/x", Err: inner}); got != inner { //nolint:errorlint // the url.Error wrapper itself must be gone
		t.Errorf("withoutURL(url.Error) = %v, want %v", got, inner)
	}
	ge := &googleapi.Error{Code: 412}
	if got := withoutURL(ge); got != ge { //nolint:errorlint // the same error must come back, not a wrapper
		t.Errorf("withoutURL(googleapi.Error) = %v, want it unchanged", got)
	}
}

func TestCanHostVM(t *testing.T) {
	tests := []struct {
		purpose string
		want    bool
	}{
		{"", true},
		{"PRIVATE", true},
		{"PRIVATE_RFC_1918", true},
		{"REGIONAL_MANAGED_PROXY", false},
		{"GLOBAL_MANAGED_PROXY", false},
		{"INTERNAL_HTTPS_LOAD_BALANCER", false},
		{"PRIVATE_SERVICE_CONNECT", false},
		{"PRIVATE_NAT", false},
		{"PEER_MIGRATION", false},
	}
	for _, tc := range tests {
		s := &computepb.Subnetwork{}
		if tc.purpose != "" {
			s.Purpose = new(tc.purpose)
		}
		if got := canHostVM(s); got != tc.want {
			t.Errorf("canHostVM(purpose=%q) = %v, want %v", tc.purpose, got, tc.want)
		}
	}
}

func operationError(code, message string) error {
	opErr := &computepb.Error{Errors: []*computepb.Errors{{Code: new(code), Message: new(message)}}}
	err, _ := apierror.FromError(&googleapi.Error{Code: 503, Message: fmt.Sprintf("%s: %v", "Service Unavailable", opErr)})
	return fmt.Errorf("create vm wait: %w", err)
}

func TestIsCapacityError(t *testing.T) {
	tests := []struct {
		desc string
		err  error
		want bool
	}{
		{desc: "operation out of capacity", err: operationError("ZONE_RESOURCE_POOL_EXHAUSTED", "The zone 'projects/p/zones/a' does not have enough resources available to fulfill the request."), want: true},
		{desc: "operation out of capacity with details", err: operationError("ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "The zone 'projects/p/zones/a' does not have enough resources available to fulfill the request."), want: true},
		{desc: "insert rejected for capacity", err: &googleapi.Error{Code: 503, Errors: []googleapi.ErrorItem{{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"}}}, want: true},
		{desc: "quota exceeded", err: operationError("QUOTA_EXCEEDED", "Quota 'CPUS' exceeded.")},
		{desc: "permission denied", err: &googleapi.Error{Code: 403, Message: "Required 'compute.instances.create' permission"}},
		{desc: "interrupted", err: fmt.Errorf("create vm: %w", context.Canceled)},
		{desc: "network error", err: &url.Error{Op: "Post", URL: "https://x", Err: errors.New("ZONE_RESOURCE_POOL_EXHAUSTED")}},
		{desc: "no error"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := isCapacityError(tc.err); got != tc.want {
				t.Errorf("isCapacityError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTryZones(t *testing.T) {
	full := operationError("ZONE_RESOURCE_POOL_EXHAUSTED", "The zone 'projects/p/zones/a' does not have enough resources available to fulfill the request.")
	quota := operationError("QUOTA_EXCEEDED", "Quota 'CPUS' exceeded.")
	tests := []struct {
		desc        string
		zones       []string
		errs        map[string]error
		wantZone    string
		wantErr     string
		wantCalls   []string
		wantRetries []string
	}{
		{desc: "first zone works", zones: []string{"a", "b", "c"}, wantZone: "a", wantCalls: []string{"a"}},
		{
			desc: "moves on after a capacity error", zones: []string{"a", "b", "c"},
			errs:     map[string]error{"a": full},
			wantZone: "b", wantCalls: []string{"a", "b"}, wantRetries: []string{"a>b"},
		},
		{
			desc: "every zone out of capacity", zones: []string{"a", "b", "c"},
			errs:     map[string]error{"a": full, "b": full, "c": full},
			wantZone: "c", wantErr: "zones a, b, c are out of capacity\nFix: try again later",
			wantCalls: []string{"a", "b", "c"}, wantRetries: []string{"a>b", "b>c"},
		},
		{
			desc: "stops on another error", zones: []string{"a", "b", "c"},
			errs:     map[string]error{"a": full, "b": quota},
			wantZone: "b", wantErr: "QUOTA_EXCEEDED", wantCalls: []string{"a", "b"}, wantRetries: []string{"a>b"},
		},
		{
			desc: "stops when interrupted", zones: []string{"a", "b", "c"},
			errs:     map[string]error{"a": context.Canceled},
			wantZone: "a", wantErr: "context canceled", wantCalls: []string{"a"},
		},
		{
			desc: "pinned zone out of capacity", zones: []string{"a"},
			errs:     map[string]error{"a": full},
			wantZone: "a", wantErr: "zone a is out of capacity\nFix: try again later", wantCalls: []string{"a"},
		},
		{desc: "no zones", wantErr: "no zones to try"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			var calls, retries []string
			zone, err := tryZones(tc.zones, func(zone string) error {
				calls = append(calls, zone)
				return tc.errs[zone]
			}, func(zone, next string) {
				retries = append(retries, zone+">"+next)
			})
			if zone != tc.wantZone {
				t.Errorf("zone = %q, want %q", zone, tc.wantZone)
			}
			if tc.wantErr == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tc.wantCalls)
			}
			if !slices.Equal(retries, tc.wantRetries) {
				t.Errorf("retries = %v, want %v", retries, tc.wantRetries)
			}
		})
	}
}

func TestNoCapacityErrorIsDefinite(t *testing.T) {
	_, err := tryZones([]string{"a", "b"}, func(string) error {
		return operationError("ZONE_RESOURCE_POOL_EXHAUSTED", "The zone 'projects/p/zones/a' does not have enough resources available to fulfill the request.")
	}, func(string, string) {})
	if MayExistAfter(err) {
		t.Errorf("MayExistAfter(%v) = true, want false: no zone created the VM", err)
	}
}

func TestPolicyViolation(t *testing.T) {
	spec := Spec{Project: "p", Zone: "europe-west1-b", Subnet: "projects/host/regions/europe-west1/subnetworks/s"}
	legacy := func(msg string) error {
		return &googleapi.Error{Code: 412, Message: msg, Errors: []googleapi.ErrorItem{{Reason: "conditionNotMet", Message: msg}}}
	}
	tests := []struct {
		desc string
		err  error
		want string
	}{
		{
			desc: "trusted image projects",
			err:  legacy("Constraint constraints/compute.trustedImageProjects violated for project p. Use of images from project debian-cloud is prohibited."),
			want: "organization policy constraints/compute.trustedImageProjects blocks the bastion\nFix: ask an admin to allow projects/debian-cloud in constraints/compute.trustedImageProjects for project p",
		},
		{
			desc: "resource locations in the operation error",
			err:  operationError("CONDITION_NOT_MET", "Location ZONE:europe-west1-b violates constraint constraints/gcp.resourceLocations on the resource projects/p/zones/europe-west1-b/instances/bqto-1."),
			want: "organization policy constraints/gcp.resourceLocations blocks the bastion\nFix: pass --zone with an allowed zone, or ask an admin to allow europe-west1-b in constraints/gcp.resourceLocations",
		},
		{
			desc: "shared VPC subnetworks",
			err:  legacy("Constraint constraints/compute.restrictSharedVpcSubnetworks violated for project p."),
			want: "organization policy constraints/compute.restrictSharedVpcSubnetworks blocks the bastion\nFix: ask an admin to allow projects/host/regions/europe-west1/subnetworks/s in constraints/compute.restrictSharedVpcSubnetworks",
		},
		{
			desc: "confidential computing",
			err:  legacy("Constraint constraints/compute.restrictNonConfidentialComputing violated for project p."),
			want: "organization policy constraints/compute.restrictNonConfidentialComputing blocks the bastion\nFix: ask an admin to exempt project p from constraints/compute.restrictNonConfidentialComputing, because bqto cannot create Confidential VMs",
		},
		{
			desc: "CMEK",
			err:  legacy("Constraint constraints/gcp.restrictNonCmekServices violated for projects/p attempting to create a resource not protected by CMEK crypto key."),
			want: "organization policy constraints/gcp.restrictNonCmekServices blocks the bastion\nFix: ask an admin to exempt project p from constraints/gcp.restrictNonCmekServices, because bqto cannot encrypt the bastion's disk with a customer key",
		},
		{
			desc: "two constraints give one Fix line",
			err:  legacy("Constraint constraints/compute.trustedImageProjects violated for project p. Constraint constraints/compute.restrictNonConfidentialComputing violated for project p."),
			want: "organization policy constraints/compute.trustedImageProjects, constraints/compute.restrictNonConfidentialComputing blocks the bastion\nFix: ask an admin to allow projects/debian-cloud in constraints/compute.trustedImageProjects for project p\n  and ask an admin to exempt project p from constraints/compute.restrictNonConfidentialComputing, because bqto cannot create Confidential VMs",
		},
		{
			desc: "unknown legacy constraint",
			err:  legacy("Constraint constraints/compute.requireShieldedVm violated for project p. Secure Boot is not enabled in the 'shielded_instance_config' field."),
			want: "organization policy constraints/compute.requireShieldedVm blocks the bastion\nFix: ask an admin for an exception to constraints/compute.requireShieldedVm for project p",
		},
		{
			desc: "custom and managed constraints",
			err:  operationError("CONDITION_NOT_MET", "Operation denied by org policies: [customConstraints/custom.allowedMachineTypes, constraints/compute.managed.disallowGlobalDns]: Only n2 machine types are allowed."),
			want: "organization policy customConstraints/custom.allowedMachineTypes, constraints/compute.managed.disallowGlobalDns blocks the bastion\nFix: ask an admin for an exception to customConstraints/custom.allowedMachineTypes for project p\n  and ask an admin to exempt project p from constraints/compute.managed.disallowGlobalDns, because bqto does not set zonal DNS on the bastion",
		},
		{
			desc: "VPC Service Controls",
			err:  &googleapi.Error{Code: 403, Message: "Request is prohibited by organization's policy. vpcServiceControlsUniqueIdentifier: AbC-123_x"},
			want: "VPC Service Controls blocks the Compute Engine API from this machine (vpcServiceControlsUniqueIdentifier: AbC-123_x)\nFix: ask an admin for an ingress rule that lets your identity call compute.googleapis.com and iaptunnel.googleapis.com",
		},
		{desc: "permission denied", err: &googleapi.Error{Code: 403, Message: "Required 'compute.instances.create' permission for 'projects/p/zones/europe-west1-b/instances/bqto-1'"}},
		{desc: "not an API error", err: errors.New("constraints/gcp.resourceLocations")},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := policyViolation(spec, tc.err)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("policyViolation() = %q, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("policyViolation() = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("policyViolation() =\n%s\nwant\n%s", err, tc.want)
			}
			if MayExistAfter(err) {
				t.Errorf("MayExistAfter(%q) = true, want false", err)
			}
		})
	}
}

func withProFlags(t *testing.T, names ...string) {
	t.Helper()
	free := proFlag
	proFlag = func(name string) bool { return slices.Contains(names, name) }
	t.Cleanup(func() { proFlag = free })
}

func TestABadMachineTypeListsTheZoneTypes(t *testing.T) {
	apiErr := &googleapi.Error{Code: 400, Message: "Invalid value for field 'resource.machineType': 'zones/us-central1-a/machineTypes/e2-micro'. Machine type with name 'e2-micro' does not exist in zone 'us-central1-a'."}
	zones := `pass --zone with a zone of us-central1 that offers e2-micro; gcloud compute machine-types list --filter="name=e2-micro AND zone~^us-central1-" lists them`
	tests := []struct {
		desc        string
		pro         []string
		machineType string
		want        string
	}{
		{"free", nil, "", "\nFix: " + zones},
		{"pro, default type", []string{"machine-type"}, "", "\nFix: " + zones + ", or pass --machine-type with another type"},
		{"pro, chosen type", []string{"machine-type"}, "e2-tiny", "\nFix: gcloud compute machine-types list --zones=us-central1-a lists the types, then pass one with --machine-type"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			withProFlags(t, tc.pro...)
			spec := Spec{Project: "p", Name: "bqto-x", Zone: "us-central1-a", MachineType: tc.machineType}
			err := createError(spec, "insert VM", apiErr)
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) || strings.Count(err.Error(), "\nFix: ") != 1 {
				t.Errorf("err = %v, want it to end with %q", err, tc.want)
			}
		})
	}
}

func TestFixesOfferAProFlagOnlyWhenItIsRegistered(t *testing.T) {
	spec := Spec{Project: "p", Subnet: "projects/host/regions/europe-west1/subnetworks/s"}
	policy := &googleapi.Error{Code: 412, Message: "Constraint constraints/compute.restrictSharedVpcSubnetworks violated for project p."}
	capacity := &noCapacityError{zones: []string{"a", "b"}}
	tests := []struct {
		desc string
		pro  []string
		got  func() string
		want string
	}{
		{"subnet policy, free", nil, func() string { return policyViolation(spec, policy).Error() }, "\nFix: ask an admin to allow projects/host/regions/europe-west1/subnetworks/s in constraints/compute.restrictSharedVpcSubnetworks"},
		{"subnet policy, pro", []string{"subnet"}, func() string { return policyViolation(spec, policy).Error() }, "\nFix: pass --subnet with an allowed subnet, or ask an admin to allow projects/host/regions/europe-west1/subnetworks/s in constraints/compute.restrictSharedVpcSubnetworks"},
		{"capacity, free", nil, capacity.Error, "\nFix: try again later"},
		{"capacity, pro", []string{"machine-type"}, capacity.Error, "\nFix: try again later, or pass --machine-type with another machine type"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			withProFlags(t, tc.pro...)
			if got := tc.got(); !strings.HasSuffix(got, tc.want) {
				t.Errorf("got %q, want it to end with %q", got, tc.want)
			}
		})
	}
}

func TestAnUnreadableAPISubnetIsABqtoBug(t *testing.T) {
	_, _, err := SubnetNetwork(context.Background(), "not-a-path")
	if err == nil || !strings.HasSuffix(err.Error(), "\nFix: this is a bqto bug; report it with this message") {
		t.Errorf("err = %v, want the bqto bug fix", err)
	}
}
