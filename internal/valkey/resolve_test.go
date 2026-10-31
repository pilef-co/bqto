package valkey

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/internal/gcpfake"
)

func TestResolveAgainstTheAPI(t *testing.T) {
	const path = "/v1/projects/p/locations/us-central1/instances/cache"

	t.Run("the primary endpoint", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", path, map[string]any{
			"name": "projects/p/locations/us-central1/instances/cache",
			"mode": "CLUSTER_DISABLED",
			"endpoints": []map[string]any{{"connections": []map[string]any{
				{"pscAutoConnection": map[string]any{"ipAddress": "10.0.0.9", "port": 6379, "network": "projects/p/global/networks/vpc", "projectId": "p", "connectionType": "CONNECTION_TYPE_READER"}},
				{"pscAutoConnection": map[string]any{"ipAddress": "10.0.0.5", "port": 6379, "network": "projects/p/global/networks/vpc", "projectId": "p", "connectionType": "CONNECTION_TYPE_PRIMARY"}},
			}}},
		})
		got, err := Resolve(context.Background(), "p", "us-central1/cache")
		want := Endpoint{IP: "10.0.0.5", Port: 6379, Region: "us-central1", NetworkProject: "p", Network: "vpc"}
		if err != nil || got != want {
			t.Errorf("Resolve = %+v, %v; want %+v", got, err, want)
		}
	})

	t.Run("a brief outage is retried", func(t *testing.T) {
		delays := gcperr.RetryDelays
		gcperr.RetryDelays = []time.Duration{0, 0, 0}
		t.Cleanup(func() { gcperr.RetryDelays = delays })
		fake := gcpfake.Start(t, &clientOptions).
			On("GET", path, gcpfake.Error(http.StatusServiceUnavailable, "backendError", "The service is currently unavailable.")).
			OK("GET", path, map[string]any{
				"name": "projects/p/locations/us-central1/instances/cache",
				"mode": "CLUSTER_DISABLED",
				"endpoints": []map[string]any{{"connections": []map[string]any{
					{"pscAutoConnection": map[string]any{"ipAddress": "10.0.0.5", "port": 6379, "network": "projects/p/global/networks/vpc", "projectId": "p", "connectionType": "CONNECTION_TYPE_PRIMARY"}},
				}}},
			})
		got, err := Resolve(context.Background(), "p", "us-central1/cache")
		if err != nil || got.IP != "10.0.0.5" || len(fake.Requests()) < 2 {
			t.Errorf("Resolve = %+v, %v after %d calls; want success after a retry", got, err, len(fake.Requests()))
		}
	})

	t.Run("not found", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("GET", path, http.StatusNotFound, "notFound", "not found")
		_, err := Resolve(context.Background(), "p", "us-central1/cache")
		if err == nil || !strings.Contains(err.Error(), ListCmd("p")) {
			t.Errorf("err = %v, want the command that lists them", err)
		}
	})

	t.Run("a cluster-mode instance named without a region", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{"instances": []map[string]any{{"name": "projects/p/locations/us-central1/instances/cache"}}}).
			OK("GET", path, map[string]any{
				"name":               "projects/p/locations/us-central1/instances/cache",
				"mode":               "CLUSTER",
				"discoveryEndpoints": []map[string]any{{"address": "10.0.0.7", "port": 6379, "network": "projects/p/global/networks/vpc"}},
			})
		_, err := Resolve(context.Background(), "p", "cache")
		if nf, ok := errors.AsType[ext.NeedsFeature](err); !ok || nf.Feature != "valkey-cluster" {
			t.Errorf("err = %v, want the valkey-cluster feature: a cluster-mode instance is the Pro variant", err)
		}
	})

	t.Run("a bare name in no region", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{})
		if _, err := Resolve(context.Background(), "p", "cache"); err == nil || !strings.Contains(err.Error(), ListCmd("p")) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a bare name in two regions", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{"instances": []map[string]any{
			{"name": "projects/p/locations/us-central1/instances/cache"}, {"name": "projects/p/locations/europe-west1/instances/cache"},
		}})
		if _, err := Resolve(context.Background(), "p", "cache"); err == nil || !strings.Contains(err.Error(), "use REGION/INSTANCE") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestTheFreeBinaryLeavesClusterInstancesOutOfAnAmbiguousName(t *testing.T) {
	gcpfake.Start(t, &clientOptions).OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{"instances": []map[string]any{
		{"name": "projects/p/locations/us-central1/instances/cache"},
		{"name": "projects/p/locations/europe-west1/instances/cache", "mode": "CLUSTER"},
	}})
	full, err := resolveRef(context.Background(), "p", "cache")
	if err != nil || full != "projects/p/locations/us-central1/instances/cache" {
		t.Errorf("resolveRef = %q, %v; want the standalone instance", full, err)
	}
}

func TestAClusterOnlyNameStillReachesTheProCheck(t *testing.T) {
	gcpfake.Start(t, &clientOptions).OK("GET", "/v1/projects/p/locations/-/instances", map[string]any{"instances": []map[string]any{
		{"name": "projects/p/locations/europe-west1/instances/cache", "mode": "CLUSTER"},
	}})
	full, err := resolveRef(context.Background(), "p", "cache")
	if err != nil || full != "projects/p/locations/europe-west1/instances/cache" {
		t.Errorf("resolveRef = %q, %v; want the cluster, so Resolve answers that it needs bqto Pro", full, err)
	}
}
