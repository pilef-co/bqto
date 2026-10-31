package alloydb

import (
	"strings"
	"testing"

	"google.golang.org/api/alloydb/v1"
)

const instName = "projects/p/locations/europe-west1/clusters/c/instances/i"

func cluster(network string) *alloydb.Cluster {
	return &alloydb.Cluster{NetworkConfig: &alloydb.NetworkConfig{Network: network}}
}

func TestEndpointFrom(t *testing.T) {
	psc := func(status, ip, network string) *alloydb.PscInstanceConfig {
		return &alloydb.PscInstanceConfig{
			PscAutoConnections: []*alloydb.PscAutoConnectionConfig{
				{ConsumerNetworkStatus: status, IpAddress: ip, ConsumerNetwork: network},
			},
		}
	}

	tests := []struct {
		desc    string
		inst    *alloydb.Instance
		cluster *alloydb.Cluster
		want    Endpoint
		wantErr string
	}{
		{
			desc:    "psc auto connection",
			inst:    &alloydb.Instance{Name: instName, PscInstanceConfig: psc("VALID", "10.0.0.5", "projects/host-p/global/networks/vpc")},
			cluster: cluster("projects/123/global/networks/other"),
			want:    Endpoint{IP: "10.0.0.5", Port: 5432, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc"},
		},
		{
			desc:    "psc connection not valid yet falls back to the private IP",
			inst:    &alloydb.Instance{Name: instName, IpAddress: "10.1.0.7", PscInstanceConfig: psc("CONNECTION_POLICY_MISSING", "10.0.0.5", "projects/host-p/global/networks/vpc")},
			cluster: cluster("projects/123/global/networks/vpc"),
			want:    Endpoint{IP: "10.1.0.7", Port: 5432, Region: "europe-west1", NetworkProject: "123", Network: "vpc"},
		},
		{
			desc:    "private IP with the cluster network",
			inst:    &alloydb.Instance{Name: instName, IpAddress: "10.1.0.7"},
			cluster: cluster("projects/123/global/networks/vpc"),
			want:    Endpoint{IP: "10.1.0.7", Port: 5432, Region: "europe-west1", NetworkProject: "123", Network: "vpc"},
		},
		{
			desc:    "managed connection pooling uses 6432",
			inst:    &alloydb.Instance{Name: instName, IpAddress: "10.1.0.7", ConnectionPoolConfig: &alloydb.ConnectionPoolConfig{Enabled: true}},
			cluster: cluster("projects/123/global/networks/vpc"),
			want:    Endpoint{IP: "10.1.0.7", Port: 6432, Region: "europe-west1", NetworkProject: "123", Network: "vpc"},
		},
		{
			desc:    "read pool resolves like the primary",
			inst:    &alloydb.Instance{Name: instName, InstanceType: "READ_POOL", IpAddress: "10.1.0.9"},
			cluster: cluster("projects/123/global/networks/vpc"),
			want:    Endpoint{IP: "10.1.0.9", Port: 5432, Region: "europe-west1", NetworkProject: "123", Network: "vpc"},
		},
		{
			desc:    "no private endpoint",
			inst:    &alloydb.Instance{Name: instName},
			cluster: cluster("projects/123/global/networks/vpc"),
			wantErr: "no private endpoint: no private IP and no active Private Service Connect connection\nFix: give the cluster a private IP",
		},
		{
			desc:    "cluster without a network",
			inst:    &alloydb.Instance{Name: instName, IpAddress: "10.1.0.7"},
			cluster: cluster(""),
			wantErr: "unexpected network path",
		},
		{
			desc:    "unexpected instance name",
			inst:    &alloydb.Instance{Name: "instances/i"},
			cluster: cluster("projects/123/global/networks/vpc"),
			wantErr: "unexpected instance name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := endpointFrom(tc.inst, tc.cluster)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("endpoint = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRefFrom(t *testing.T) {
	got, err := refFrom(instName)
	if err != nil || got != "europe-west1/c/i" {
		t.Errorf("refFrom = %q, %v; want \"europe-west1/c/i\", nil", got, err)
	}
	if _, err := refFrom("projects/p/locations/r/clusters/c"); err == nil {
		t.Error("refFrom of a cluster name = nil error, want an error")
	}
}

func TestMatchRefs(t *testing.T) {
	refs := []string{"europe-west1/c1/i", "us-central1/c2/i", "us-central1/c1/other", "us-central1/c3/india"}
	tests := []struct {
		desc string
		ref  string
		want []string
	}{
		{desc: "full ref", ref: "us-central1/c2/i", want: []string{"us-central1/c2/i"}},
		{desc: "cluster and instance", ref: "c1/i", want: []string{"europe-west1/c1/i"}},
		{desc: "instance name in two clusters", ref: "i", want: []string{"europe-west1/c1/i", "us-central1/c2/i"}},
		{desc: "no match", ref: "missing"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := matchRefs(refs, tc.ref)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("matchRefs(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func TestChoiceFor(t *testing.T) {
	tests := []struct {
		desc    string
		inst    *alloydb.Instance
		want    Choice
		wantErr bool
	}{
		{desc: "primary", inst: &alloydb.Instance{Name: instName, InstanceType: "PRIMARY"}, want: Choice{Value: "europe-west1/c/i", Label: "europe-west1/c/i"}},
		{desc: "read pool", inst: &alloydb.Instance{Name: instName, InstanceType: "READ_POOL"}, want: Choice{Value: "europe-west1/c/i", Label: "europe-west1/c/i (read pool)"}},
		{desc: "cross-region replica", inst: &alloydb.Instance{Name: instName, InstanceType: "SECONDARY"}, want: Choice{Value: "europe-west1/c/i", Label: "europe-west1/c/i (cross-region replica)"}},
		{desc: "unexpected name", inst: &alloydb.Instance{Name: "instances/i"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := choiceFor(tc.inst)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("choiceFor() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestListCmd(t *testing.T) {
	if got, want := ListCmd("p"), "gcloud alloydb clusters list --project p --region -"; got != want {
		t.Errorf("ListCmd = %q, want %q", got, want)
	}
}

func TestClusterRef(t *testing.T) {
	cases := []struct{ in, want string }{
		{"europe-west1/c1/i1", "europe-west1/c1"},
		{"i1", "i1"},
		{"c1/i1", "c1/i1"},
	}
	for _, c := range cases {
		if got := clusterRef(c.in); got != c.want {
			t.Errorf("clusterRef(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
