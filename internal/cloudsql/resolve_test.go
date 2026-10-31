package cloudsql

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/internal/gcpfake"
	"github.com/pilef-co/bqto/quota"
)

func TestResolveAgainstTheAPI(t *testing.T) {
	const path = "/v1/projects/p/instances/sql"

	t.Run("private IP", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", path, map[string]any{
			"name":            "sql",
			"region":          "us-central1",
			"databaseVersion": "POSTGRES_16",
			"ipAddresses":     []map[string]any{{"type": "PRIMARY", "ipAddress": "34.1.2.3"}, {"type": "PRIVATE", "ipAddress": "10.10.0.3"}},
			"settings":        map[string]any{"ipConfiguration": map[string]any{"privateNetwork": "projects/host-p/global/networks/vpc"}},
		})
		got, err := Resolve(context.Background(), "p", "sql")
		if err != nil {
			t.Fatal(err)
		}
		want := Endpoint{IP: "10.10.0.3", Port: 5432, Region: "us-central1", NetworkProject: "host-p", Network: "vpc"}
		if got != want {
			t.Errorf("Resolve = %+v, want %+v", got, want)
		}
	})

	t.Run("not found", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("GET", path, http.StatusNotFound, "notFound", "The Cloud SQL instance does not exist.")
		_, err := Resolve(context.Background(), "p", "sql")
		if err == nil || !strings.Contains(err.Error(), "gcloud sql instances list --project p") {
			t.Errorf("err = %v, want the command that lists the instances", err)
		}
	})
}

func TestTheInstancesProjectIsTheQuotaProject(t *testing.T) {
	const path = "/v1/projects/prj-app/instances/sql"
	instance := map[string]any{
		"name":            "sql",
		"region":          "us-central1",
		"databaseVersion": "POSTGRES_16",
		"ipAddresses":     []map[string]any{{"type": "PRIVATE", "ipAddress": "10.10.0.3"}},
		"settings":        map[string]any{"ipConfiguration": map[string]any{"privateNetwork": "projects/prj-app/global/networks/vpc"}},
	}
	ctx := quota.WithProject(context.Background(), "prj-app")

	t.Run("the API is checked in the instance's project", func(t *testing.T) {
		gcpfake.UserADC(t)
		fake := gcpfake.Start(t, &clientOptions).OK("GET", path, instance)
		if _, err := Resolve(ctx, "prj-app", "sql"); err != nil {
			t.Fatal(err)
		}
		if got := fake.Requests()[0].UserProject; got != "prj-app" {
			t.Errorf("X-Goog-User-Project = %q, want prj-app", got)
		}
	})

	t.Run("a project the account cannot bill names it in the Fix", func(t *testing.T) {
		gcpfake.UserADC(t)
		gcpfake.Start(t, &clientOptions).On("GET", path,
			gcpfake.ErrorInfo(http.StatusForbidden, "USER_PROJECT_DENIED", "Caller does not have required permission to use project prj-app.",
				map[string]string{"consumer": "projects/prj-app", "service": "sqladmin.googleapis.com"}),
			gcpfake.ErrorInfo(http.StatusForbidden, "SERVICE_DISABLED", "Cloud SQL Admin API has not been used in project prj-quota-sandbox before or it is disabled.",
				map[string]string{"consumer": "projects/prj-quota-sandbox", "service": "sqladmin.googleapis.com"}),
		)
		_, err := Resolve(ctx, "prj-app", "sql")
		if err == nil || !strings.Contains(err.Error(), "Fix: your account cannot use project prj-app for API checks and quota") || strings.Contains(err.Error(), "prj-quota-sandbox") {
			t.Errorf("err = %v, want the Fix for prj-app only", err)
		}
	})

	t.Run("a refusing target is retried without its project", func(t *testing.T) {
		gcpfake.UserADC(t)
		viewerOnly := fmt.Sprintf("prj-viewer-only-%d", time.Now().UnixNano())
		fake := gcpfake.Start(t, &clientOptions).On("GET", path,
			gcpfake.ErrorInfo(http.StatusForbidden, "USER_PROJECT_DENIED", "Caller does not have required permission to use project "+viewerOnly+".",
				map[string]string{"consumer": "projects/" + viewerOnly}),
			gcpfake.Response{Status: http.StatusOK, Body: instance},
		)
		viewer := quota.WithProject(context.Background(), viewerOnly)
		if _, err := Resolve(viewer, "prj-app", "sql"); err != nil {
			t.Fatal(err)
		}
		if got := fake.Requests(); len(got) != 2 || got[0].UserProject != viewerOnly || got[1].UserProject != "" {
			t.Errorf("requests = %+v, want the target, then the ADC default", got)
		}
	})
}
