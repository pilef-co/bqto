package redis

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/internal/gcpfake"
)

func TestResolveAgainstTheAPI(t *testing.T) {
	const (
		instName    = "projects/p/locations/us-central1/instances/cache"
		clusterName = "projects/p/locations/us-central1/clusters/cache"
		network     = "projects/host-p/global/networks/vpc"
	)
	instance := map[string]any{"name": instName, "host": "10.0.0.3", "port": 6379, "authorizedNetwork": network}
	instanceEP := Endpoint{IP: "10.0.0.3", Port: 6379, Region: "us-central1", NetworkProject: "host-p", Network: "vpc"}
	cluster := map[string]any{
		"name":                  clusterName,
		"transitEncryptionMode": "TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION",
		"discoveryEndpoints":    []map[string]any{{"address": "10.0.0.9", "port": 6379, "pscConfig": map[string]any{"network": network}}},
	}
	listed := func(fake *gcpfake.Server, instances, clusters []string) {
		items := func(names []string) []map[string]any {
			out := []map[string]any{}
			for _, n := range names {
				out = append(out, map[string]any{"name": n})
			}
			return out
		}
		fake.OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{"instances": items(instances)})
		if clusters != nil {
			fake.OK("GET", "/v1/projects/p/locations/-/clusters", map[string]any{"clusters": items(clusters)})
		}
	}

	tests := []struct {
		desc    string
		ref     string
		fake    func(*gcpfake.Server)
		want    Endpoint
		wantErr string
	}{
		{
			desc: "an instance",
			ref:  "us-central1/cache",
			fake: func(f *gcpfake.Server) { f.OK("GET", "/v1/"+instName, instance) },
			want: instanceEP,
		},
		{
			desc: "not an instance but a cluster, which needs the Pro variant",
			ref:  "us-central1/cache",
			fake: func(f *gcpfake.Server) {
				f.Err("GET", "/v1/"+instName, http.StatusNotFound, "notFound", "not found").OK("GET", "/v1/"+clusterName, cluster)
			},
			wantErr: "us-central1/cache needs redis-cluster",
		},
		{
			desc: "neither",
			ref:  "us-central1/cache",
			fake: func(f *gcpfake.Server) {
				f.Err("GET", "/v1/"+instName, http.StatusNotFound, "notFound", "not found").
					Err("GET", "/v1/"+clusterName, http.StatusNotFound, "notFound", "not found")
			},
			wantErr: ListCmd("p"),
		},
		{
			desc: "a name without a region is found by listing",
			ref:  "cache",
			fake: func(f *gcpfake.Server) {
				listed(f, []string{instName}, nil)
				f.OK("GET", "/v1/"+instName, instance)
			},
			want: instanceEP,
		},
		{
			desc: "a cluster named without a region",
			ref:  "cache",
			fake: func(f *gcpfake.Server) {
				listed(f, nil, []string{clusterName})
				f.OK("GET", "/v1/"+clusterName, cluster)
			},
			wantErr: "us-central1/cache needs redis-cluster",
		},
		{
			desc: "the free binary never offers a cluster when an instance has the name",
			ref:  "cache",
			fake: func(f *gcpfake.Server) {
				listed(f, []string{instName}, nil)
				f.OK("GET", "/v1/"+instName, instance)
			},
			want: instanceEP,
		},
		{
			desc:    "the full name reaches the cluster",
			ref:     clusterName,
			fake:    func(f *gcpfake.Server) { f.OK("GET", "/v1/"+clusterName, cluster) },
			wantErr: "us-central1/cache needs redis-cluster",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			tc.fake(gcpfake.Start(t, &clientOptions))
			got, err := Resolve(context.Background(), "p", tc.ref)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("Resolve = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestWithClustersAnInstanceAndAClusterWithTheSameNameAreBothOffered(t *testing.T) {
	open := clustersOpen
	clustersOpen = func() bool { return true }
	t.Cleanup(func() { clustersOpen = open })
	instName := "projects/p/locations/us-central1/instances/cache"
	clusterName := "projects/p/locations/us-central1/clusters/cache"
	gcpfake.Start(t, &clientOptions).
		OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{"instances": []map[string]any{{"name": instName}}}).
		OK("GET", "/v1/projects/p/locations/-/clusters", map[string]any{"clusters": []map[string]any{{"name": clusterName}}})
	_, err := Resolve(context.Background(), "p", "cache")
	if err == nil || !strings.Contains(err.Error(), "pass one of: "+clusterName+", "+instName) {
		t.Errorf("err = %v, want both names", err)
	}
}

func TestABadRefNamesClustersOnlyWhenTheBinaryHasThem(t *testing.T) {
	if err := CheckRef("projects/p/bad"); err == nil || strings.Contains(err.Error(), "clusters") {
		t.Errorf("free: err = %v, want no cluster form", err)
	}
	open := clustersOpen
	clustersOpen = func() bool { return true }
	t.Cleanup(func() { clustersOpen = open })
	if err := CheckRef("projects/p/bad"); err == nil || !strings.Contains(err.Error(), "clusters/N") {
		t.Errorf("pro: err = %v, want the cluster form", err)
	}
}
