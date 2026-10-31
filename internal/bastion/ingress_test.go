package bastion

import (
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
)

func TestInsideIAP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src  string
		want bool
	}{
		{src: "35.235.240.0/20", want: true},
		{src: "35.235.240.0/24", want: true},
		{src: "35.235.240.5", want: true},
		{src: "0.0.0.0/0"},
		{src: "35.235.240.0/16"},
		{src: "10.0.0.0/8"},
		{src: "::/0"},
		{src: "::ffff:35.235.240.0/100"},
		{src: "::ffff:35.235.240.0/40"},
		{src: "nonsense"},
	}
	for _, tc := range tests {
		if got := insideIAP(tc.src); got != tc.want {
			t.Errorf("insideIAP(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestOtherSources(t *testing.T) {
	t.Parallel()
	vpc := vpcOtherSources(&computepb.Firewall{SourceTags: []string{"jump"}, SourceServiceAccounts: []string{"ci@p.iam.gserviceaccount.com"}})
	if want := []string{"source tags jump", "source service accounts ci@p.iam.gserviceaccount.com"}; !slices.Equal(vpc, want) {
		t.Errorf("vpcOtherSources = %v, want %v", vpc, want)
	}
	policy := policyOtherSources(&computepb.FirewallPolicyRuleMatcher{
		SrcIpRanges:    []string{"35.235.240.0/20"},
		SrcRegionCodes: []string{"US"},
		SrcSecureTags:  []*computepb.FirewallPolicyRuleSecureTag{{Name: new("tagValues/123")}},
	})
	if want := []string{"secure tags tagValues/123", "regions US"}; !slices.Equal(policy, want) {
		t.Errorf("policyOtherSources = %v, want %v", policy, want)
	}
	if got := vpcOtherSources(&computepb.Firewall{SourceRanges: []string{"35.235.240.0/20"}}); got != nil {
		t.Errorf("an IP-only rule has other sources %v", got)
	}
}

func TestWideIngressWarnings(t *testing.T) {
	t.Parallel()
	tcp := func(ports ...string) []protoPorts {
		return []protoPorts{&computepb.Allowed{IPProtocol: new("tcp"), Ports: ports}}
	}

	tests := []struct {
		desc    string
		sources []string
		others  []string
		ports   []protoPorts
		want    []string
	}{
		{
			desc:    "the IAP range and every VM tagged jump",
			sources: []string{"35.235.240.0/20"},
			others:  []string{"source tags jump"},
			ports:   tcp("22"),
			want:    []string{"also allows tcp:22 from source tags jump"},
		},
		{
			desc:    "exactly the IAP range on tcp:22",
			sources: []string{"35.235.240.0/20"},
			ports:   tcp("22"),
		},
		{
			desc:    "open to the internet",
			sources: []string{"35.235.240.0/20", "0.0.0.0/0"},
			ports:   tcp("22"),
			want:    []string{"also allows tcp:22 from 0.0.0.0/0"},
		},
		{
			desc:    "every port",
			sources: []string{"35.235.240.0/20"},
			ports:   []protoPorts{&computepb.Allowed{IPProtocol: new("all")}},
			want:    []string{"opens more than tcp:22"},
		},
		{
			desc:    "a range of ports",
			sources: []string{"35.235.240.0/20"},
			ports:   tcp("20-30"),
			want:    []string{"opens more than tcp:22"},
		},
		{
			desc:    "tcp:3389 in its own entry, as Terraform writes two allow blocks",
			sources: []string{"35.235.240.0/20"},
			ports:   append(tcp("22"), tcp("3389")...),
			want:    []string{"opens more than tcp:22"},
		},
		{
			desc:    "22-22 is only port 22",
			sources: []string{"35.235.240.0/20"},
			ports:   tcp("22-22"),
		},
		{
			desc:    "22 listed twice",
			sources: []string{"35.235.240.0/20"},
			ports:   tcp("22", "22"),
		},
		{
			desc:    "every tcp port",
			sources: []string{"35.235.240.0/20"},
			ports:   tcp(),
			want:    []string{"opens more than tcp:22"},
		},
		{
			desc:    "another protocol that cannot reach sshd",
			sources: []string{"35.235.240.0/20"},
			ports:   []protoPorts{&computepb.Allowed{IPProtocol: new("udp"), Ports: []string{"53"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := wideIngressWarnings(`VPC firewall rule "allow-iap"`, tc.sources, tc.others, tc.ports)
			if len(got) != len(tc.want) {
				t.Fatalf("warnings = %v, want %d of them", got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning %q does not contain %q", got[i], w)
				}
			}
		})
	}
}
