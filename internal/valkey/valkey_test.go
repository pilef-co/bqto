package valkey

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/memorystore/apiv1/memorystorepb"
)

const instName = "projects/p/locations/europe-west1/instances/cache"

func endpoints(conns ...*memorystorepb.Instance_ConnectionDetail) []*memorystorepb.Instance_InstanceEndpoint {
	return []*memorystorepb.Instance_InstanceEndpoint{{Connections: conns}}
}

func auto(kind memorystorepb.ConnectionType, ip, network, project string) *memorystorepb.Instance_ConnectionDetail {
	return &memorystorepb.Instance_ConnectionDetail{
		Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{
			PscAutoConnection: &memorystorepb.PscAutoConnection{
				IpAddress:      ip,
				Ports:          &memorystorepb.PscAutoConnection_Port{Port: 6379},
				Network:        network,
				ProjectId:      project,
				ConnectionType: kind,
			},
		},
	}
}

func manual(kind memorystorepb.ConnectionType, ip, network, project string) *memorystorepb.Instance_ConnectionDetail {
	return &memorystorepb.Instance_ConnectionDetail{
		Connection: &memorystorepb.Instance_ConnectionDetail_PscConnection{
			PscConnection: &memorystorepb.PscConnection{
				IpAddress:      ip,
				Ports:          &memorystorepb.PscConnection_Port{Port: 6379},
				Network:        network,
				ProjectId:      project,
				ConnectionType: kind,
			},
		},
	}
}

func TestEndpointFrom(t *testing.T) {
	const network = "projects/host-p/global/networks/vpc"
	primary := memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY
	reader := memorystorepb.ConnectionType_CONNECTION_TYPE_READER

	tests := []struct {
		desc    string
		inst    *memorystorepb.Instance
		want    Endpoint
		wantErr string
	}{
		{
			desc: "primary psc auto connection",
			inst: &memorystorepb.Instance{
				Name:      instName,
				Mode:      memorystorepb.Instance_CLUSTER_DISABLED,
				Endpoints: endpoints(auto(primary, "10.0.0.5", network, "host-p")),
			},
			want: Endpoint{IP: "10.0.0.5", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc"},
		},
		{
			desc: "reader connection is skipped",
			inst: &memorystorepb.Instance{
				Name:      instName,
				Mode:      memorystorepb.Instance_CLUSTER_DISABLED,
				Endpoints: endpoints(auto(reader, "10.0.0.9", network, "host-p"), auto(primary, "10.0.0.5", network, "host-p")),
			},
			want: Endpoint{IP: "10.0.0.5", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc"},
		},
		{
			desc: "manual psc connection",
			inst: &memorystorepb.Instance{
				Name:      instName,
				Mode:      memorystorepb.Instance_CLUSTER_DISABLED,
				Endpoints: endpoints(manual(primary, "10.0.0.7", network, "host-p")),
			},
			want: Endpoint{IP: "10.0.0.7", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc"},
		},
		{
			desc: "encryption and IAM auth",
			inst: &memorystorepb.Instance{
				Name:                  instName,
				Mode:                  memorystorepb.Instance_CLUSTER_DISABLED,
				Endpoints:             endpoints(auto(primary, "10.0.0.5", network, "host-p")),
				TransitEncryptionMode: memorystorepb.Instance_SERVER_AUTHENTICATION,
				AuthorizationMode:     memorystorepb.Instance_IAM_AUTH,
			},
			want: Endpoint{IP: "10.0.0.5", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc", TLS: true, IAMAuth: true},
		},
		{
			desc: "top-level auto connections are used too",
			inst: &memorystorepb.Instance{
				Name: instName,
				Mode: memorystorepb.Instance_CLUSTER_DISABLED,
				PscAutoConnections: []*memorystorepb.PscAutoConnection{{ //nolint:staticcheck // older instances report only this field
					IpAddress:      "10.0.0.6",
					Ports:          &memorystorepb.PscAutoConnection_Port{Port: 6379},
					Network:        network,
					ProjectId:      "host-p",
					ConnectionType: primary,
				}},
			},
			want: Endpoint{IP: "10.0.0.6", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc"},
		},
		{
			desc:    "cluster mode is the Pro variant",
			inst:    &memorystorepb.Instance{Name: instName, Mode: memorystorepb.Instance_CLUSTER},
			wantErr: "europe-west1/cache needs valkey-cluster",
		},
		{
			desc:    "no primary endpoint yet",
			inst:    &memorystorepb.Instance{Name: instName, Mode: memorystorepb.Instance_CLUSTER_DISABLED, State: memorystorepb.Instance_CREATING},
			wantErr: "no primary endpoint yet (state CREATING)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := endpointFrom(tc.inst)
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

func TestHint(t *testing.T) {
	base := Endpoint{Region: "europe-west1"}
	tests := []struct {
		desc     string
		ep       Endpoint
		wantHas  []string
		wantMiss []string
	}{
		{
			desc:     "plain instance",
			ep:       base,
			wantHas:  []string{"valkey-cli -h 127.0.0.1 -p 16379"},
			wantMiss: []string{"--tls", "IAM auth"},
		},
		{
			desc:     "encryption and IAM auth",
			ep:       func() Endpoint { e := base; e.TLS = true; e.IAMAuth = true; return e }(),
			wantHas:  []string{"--tls --cacert server-ca.pem", "get-certificate-authority", `REDISCLI_AUTH="$(gcloud auth print-access-token)" before the command`},
			wantMiss: []string{"the password is"},
		},
	}
	withInstalled(t, "valkey-cli")
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := Hint(tc.ep, 16379)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("hint %q does not contain %q", got, want)
				}
			}
			for _, miss := range tc.wantMiss {
				if strings.Contains(got, miss) {
					t.Errorf("hint %q should not contain %q", got, miss)
				}
			}
		})
	}
}

func TestMatchRefs(t *testing.T) {
	refs := []string{"europe-west1/cache", "us-central1/cache", "us-central1/other"}
	tests := []struct {
		desc string
		ref  string
		want []string
	}{
		{desc: "full ref", ref: "us-central1/other", want: []string{"us-central1/other"}},
		{desc: "name in two regions", ref: "cache", want: []string{"europe-west1/cache", "us-central1/cache"}},
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

func TestCLI(t *testing.T) {
	tests := []struct {
		desc      string
		installed []string
		want      string
	}{
		{desc: "valkey-cli installed", installed: []string{"valkey-cli", "redis-cli"}, want: "valkey-cli"},
		{desc: "only redis-cli, which speaks the same protocol", installed: []string{"redis-cli"}, want: "redis-cli"},
		{desc: "neither", want: "valkey-cli"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			withInstalled(t, tc.installed...)
			if got := cli(); got != tc.want {
				t.Errorf("cli() = %q, want %q", got, tc.want)
			}
		})
	}
}

func withInstalled(t *testing.T, names ...string) {
	t.Helper()
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if slices.Contains(names, name) {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = orig })
}

func TestHintsWithOnlyRedisCLI(t *testing.T) {
	withInstalled(t, "redis-cli")
	if got := Hint(Endpoint{Region: "europe-west1"}, 16379); !strings.Contains(got, "redis-cli -h 127.0.0.1 -p 16379") {
		t.Errorf("Hint = %q, want redis-cli", got)
	}
}

func TestListCmd(t *testing.T) {
	if got, want := ListCmd("p"), "gcloud memorystore instances list --project p --location -"; got != want {
		t.Errorf("ListCmd = %q, want %q", got, want)
	}
}
