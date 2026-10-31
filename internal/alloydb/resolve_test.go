package alloydb

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/internal/gcpfake"
)

func TestResolveAgainstTheAPI(t *testing.T) {
	const (
		instPath    = "/v1/projects/p/locations/us-central1/clusters/c/instances/primary"
		clusterPath = "/v1/projects/p/locations/us-central1/clusters/c"
	)

	t.Run("private IP, with the network project given as a number", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", instPath, map[string]any{"name": "projects/p/locations/us-central1/clusters/c/instances/primary", "ipAddress": "10.0.0.4"}).
			OK("GET", clusterPath, map[string]any{"networkConfig": map[string]any{"network": "projects/123/global/networks/vpc"}}).
			OK("GET", "/v3/projects/123", map[string]any{"projectId": "host-p"})
		got, err := Resolve(context.Background(), "p", "us-central1/c/primary")
		want := Endpoint{IP: "10.0.0.4", Port: defaultPort, Region: "us-central1", NetworkProject: "host-p", Network: "vpc"}
		if err != nil || got != want {
			t.Errorf("Resolve = %+v, %v; want %+v", got, err, want)
		}
	})

	t.Run("not found", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("GET", instPath, http.StatusNotFound, "notFound", "not found")
		_, err := Resolve(context.Background(), "p", "us-central1/c/primary")
		if err == nil || !strings.Contains(err.Error(), ListCmd("p")) {
			t.Errorf("err = %v, want the command that lists them", err)
		}
	})

	inst := func(name string) map[string]any { return map[string]any{"name": name} }
	const listPath = "/v1/projects/p/locations/-/clusters/-/instances"

	t.Run("a bare name found by listing", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", listPath, map[string]any{"instances": []any{inst("projects/p/locations/us-central1/clusters/c/instances/primary")}}).
			OK("GET", instPath, map[string]any{"name": "projects/p/locations/us-central1/clusters/c/instances/primary", "ipAddress": "10.0.0.4"}).
			OK("GET", clusterPath, map[string]any{"networkConfig": map[string]any{"network": "projects/host-p/global/networks/vpc"}})
		got, err := Resolve(context.Background(), "p", "primary")
		want := Endpoint{IP: "10.0.0.4", Port: defaultPort, Region: "us-central1", NetworkProject: "host-p", Network: "vpc"}
		if err != nil || got != want {
			t.Errorf("Resolve = %+v, %v; want %+v", got, err, want)
		}
	})

	t.Run("a bare name in no cluster", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", listPath, map[string]any{})
		if _, err := Resolve(context.Background(), "p", "primary"); err == nil || !strings.Contains(err.Error(), "AlloyDB instance primary not found in project p\nFix: gcloud alloydb clusters list") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a bare name in two clusters", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", listPath, map[string]any{"instances": []any{
			inst("projects/p/locations/us-central1/clusters/a/instances/primary"), inst("projects/p/locations/us-central1/clusters/b/instances/primary"),
		}})
		if _, err := Resolve(context.Background(), "p", "primary"); err == nil || !strings.Contains(err.Error(), "use REGION/CLUSTER/INSTANCE") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestProjectIDNamesThePermission(t *testing.T) {
	gcpfake.Start(t, &clientOptions).Err("GET", "/v3/projects/123", http.StatusForbidden, "forbidden", "The caller does not have permission")
	_, err := projectID(context.Background(), "123")
	if err == nil || !strings.Contains(err.Error(), "network project") || !strings.HasSuffix(err.Error(), "\nFix: ask for resourcemanager.projects.get on project 123 (roles/browser)") {
		t.Errorf("err = %v, want the network project and the permission", err)
	}
}
