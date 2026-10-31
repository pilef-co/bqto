package redis

import (
	"strings"
	"testing"

	redisapi "google.golang.org/api/redis/v1"
)

const instName = "projects/p/locations/europe-west1/instances/cache"

func TestEndpointFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		desc    string
		inst    *redisapi.Instance
		want    Endpoint
		wantErr string
	}{
		{
			desc: "basic tier",
			inst: &redisapi.Instance{Name: instName, Host: "10.0.0.3", Port: 6379, AuthorizedNetwork: "projects/host-p/global/networks/vpc"},
			want: Endpoint{IP: "10.0.0.3", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc"},
		},
		{
			desc: "in-transit encryption and AUTH",
			inst: &redisapi.Instance{
				Name: instName, Host: "10.0.0.3", Port: 6378,
				AuthorizedNetwork:     "projects/host-p/global/networks/vpc",
				TransitEncryptionMode: "SERVER_AUTHENTICATION", AuthEnabled: true,
			},
			want: Endpoint{IP: "10.0.0.3", Port: 6378, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc", TLS: true, Auth: true},
		},
		{
			desc: "standard tier read endpoint",
			inst: &redisapi.Instance{
				Name: instName, Host: "10.0.0.3", Port: 6379,
				AuthorizedNetwork: "projects/host-p/global/networks/vpc",
				ReadEndpoint:      "10.0.0.4", ReadEndpointPort: 6379,
			},
			want: Endpoint{IP: "10.0.0.3", Port: 6379, Region: "europe-west1", NetworkProject: "host-p", Network: "vpc", ReadIP: "10.0.0.4", ReadPort: 6379},
		},
		{
			desc: "network given as a bare name",
			inst: &redisapi.Instance{Name: instName, Host: "10.0.0.3", Port: 6379, AuthorizedNetwork: "default"},
			want: Endpoint{IP: "10.0.0.3", Port: 6379, Region: "europe-west1", Network: "default"},
		},
		{
			desc:    "instance without an endpoint yet",
			inst:    &redisapi.Instance{Name: instName, State: "CREATING", AuthorizedNetwork: "default"},
			wantErr: "no endpoint yet (state CREATING)",
		},
		{
			desc:    "no network",
			inst:    &redisapi.Instance{Name: instName, Host: "10.0.0.3", Port: 6379},
			wantErr: "empty network",
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
	t.Parallel()
	base := Endpoint{IP: "10.0.0.3", Port: 6379, Region: "europe-west1"}
	tests := []struct {
		desc     string
		ep       Endpoint
		wantHas  []string
		wantMiss []string
	}{
		{
			desc:     "plain instance",
			ep:       base,
			wantHas:  []string{"redis-cli -h 127.0.0.1 -p 16379"},
			wantMiss: []string{"--tls", "AUTH", "read endpoint"},
		},
		{
			desc:    "tls and auth",
			ep:      func() Endpoint { e := base; e.TLS = true; e.Auth = true; return e }(),
			wantHas: []string{"--tls --cacert server-ca.pem", "gcloud redis instances describe cache --region europe-west1", "get-auth-string cache"},
		},
		{
			desc:    "read endpoint",
			ep:      func() Endpoint { e := base; e.ReadIP = "10.0.0.4"; e.ReadPort = 6379; return e }(),
			wantHas: []string{"read endpoint 10.0.0.4:6379 is not forwarded"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := Hint(tc.ep, "europe-west1/cache", 16379)
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

func TestRefFrom(t *testing.T) {
	got, err := refFrom(instName)
	if err != nil || got != "europe-west1/cache" {
		t.Errorf("refFrom = %q, %v; want \"europe-west1/cache\", nil", got, err)
	}
	if _, err := refFrom("projects/p/locations/r"); err == nil {
		t.Error("refFrom of a location = nil error, want an error")
	}
}

func TestMatchRefs(t *testing.T) {
	t.Parallel()
	names := []string{
		"projects/p/locations/europe-west1/instances/cache",
		"projects/p/locations/us-central1/instances/cache",
		"projects/p/locations/us-central1/instances/other",
		"projects/p/locations/us-central1/instances/cache-2",
	}
	tests := []struct {
		desc string
		ref  string
		want []string
	}{
		{desc: "name in two regions", ref: "cache", want: []string{names[0], names[1]}},
		{desc: "one match", ref: "other", want: []string{names[2]}},
		{desc: "no match", ref: "missing"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := matchRefs(names, tc.ref)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("matchRefs(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func TestListCmd(t *testing.T) {
	if got, want := ListCmd("p"), "gcloud redis instances list --project p --region -"; got != want {
		t.Errorf("ListCmd = %q, want %q", got, want)
	}
}
