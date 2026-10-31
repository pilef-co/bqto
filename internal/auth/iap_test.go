package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/internal/gcpfake"
	"google.golang.org/api/option"
)

func TestCheckIAPTunnelAgainstTheAPI(t *testing.T) {
	const path = "/v3/projects/p:testIamPermissions"
	const projectPath = "/v3/projects/p"
	const missingProject = "project p does not exist, or your account cannot see it\nFix: check the ID with gcloud projects list; if the project exists but is not listed, ask for resourcemanager.projects.get on it (roles/browser)"
	both := map[string]any{"permissions": []string{iapTunnelPermission, projectGetPermission}}
	visible := map[string]any{"permissions": []string{projectGetPermission}}

	t.Run("granted", func(t *testing.T) {
		fake := gcpfake.Start(t, &clientOptions).OK("POST", path, both)
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if err != nil || warning != "" {
			t.Errorf("CheckIAPTunnel = %q, %v; want no warning and no error", warning, err)
		}
		if body := fake.Requests()[0].Body; !strings.Contains(body, projectGetPermission) {
			t.Errorf("testIamPermissions body = %s, want it to ask for %s", body, projectGetPermission)
		}
	})

	t.Run("missing is a warning, not an error", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("POST", path, visible)
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if err != nil || !strings.Contains(warning, "--role=roles/iap.tunnelResourceAccessor") {
			t.Errorf("CheckIAPTunnel = %q, %v; want the grant command as a warning", warning, err)
		}
	})

	t.Run("a project hidden by conditional grants is read to confirm it exists", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("POST", path, map[string]any{}).
			OK("GET", projectPath, map[string]any{"name": "projects/123", "projectId": "p"})
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if err != nil || !strings.Contains(warning, "--role=roles/iap.tunnelResourceAccessor") {
			t.Errorf("CheckIAPTunnel = %q, %v; want the IAP warning and no error", warning, err)
		}
	})

	t.Run("a project that does not exist fails before the IAP warning", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("POST", path, map[string]any{}).
			Err("GET", projectPath, http.StatusForbidden, "forbidden", "Permission 'resourcemanager.projects.get' denied on resource '//cloudresourcemanager.googleapis.com/projects/p' (or it may not exist).")
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if warning != "" || err == nil || err.Error() != missingProject {
			t.Errorf("CheckIAPTunnel = %q, %v; want no warning and %q", warning, err, missingProject)
		}
	})

	t.Run("a project the read cannot find is missing", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("POST", path, map[string]any{}).
			Err("GET", projectPath, http.StatusNotFound, "notFound", "Project not found.")
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if warning != "" || err == nil || err.Error() != missingProject {
			t.Errorf("CheckIAPTunnel = %q, %v; want no warning and %q", warning, err, missingProject)
		}
	})

	t.Run("a disabled API on the read keeps its own fix", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("POST", path, map[string]any{}).
			On("GET", projectPath, gcpfake.Response{Status: http.StatusForbidden, Body: map[string]any{
				"error": map[string]any{
					"code": 403, "message": "Cloud Resource Manager API has not been used in project quota-p",
					"details": []map[string]any{{
						"@type":    "type.googleapis.com/google.rpc.ErrorInfo",
						"reason":   "SERVICE_DISABLED",
						"metadata": map[string]any{"consumer": "projects/quota-p", "service": "cloudresourcemanager.googleapis.com"},
					}},
				},
			}})
		_, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if err == nil || !strings.HasPrefix(err.Error(), "read project p: ") || !strings.HasSuffix(err.Error(), "\nFix: gcloud services enable cloudresourcemanager.googleapis.com --project quota-p") {
			t.Errorf("err = %v, want the enable command", err)
		}
	})

	t.Run("a failed check stays an error", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).On("POST", path, gcpfake.Response{Status: http.StatusForbidden, Body: map[string]any{
			"error": map[string]any{
				"code": 403, "message": "Cloud Resource Manager API has not been used in project quota-p",
				"details": []map[string]any{{
					"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
					"reason": "SERVICE_DISABLED",
					"metadata": map[string]any{
						"consumer": "projects/quota-p", "service": "cloudresourcemanager.googleapis.com",
						"serviceTitle": "Cloud Resource Manager API",
					},
				}},
			},
		}})
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if warning != "" || err == nil || !strings.Contains(err.Error(), "gcloud services enable cloudresourcemanager.googleapis.com --project quota-p") {
			t.Errorf("CheckIAPTunnel = %q, %v; want the enable command as an error", warning, err)
		}
	})

	t.Run("a brief outage is retried", func(t *testing.T) {
		noRetryWait(t)
		fake := gcpfake.Start(t, &clientOptions).
			On("POST", path, gcpfake.Error(http.StatusServiceUnavailable, "backendError", "The service is currently unavailable.")).
			OK("POST", path, both)
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if err != nil || warning != "" || len(fake.Requests()) != 2 {
			t.Errorf("CheckIAPTunnel = %q, %v after %d calls; want success on the second", warning, err, len(fake.Requests()))
		}
	})

	t.Run("a lasting outage asks to run again", func(t *testing.T) {
		noRetryWait(t)
		outage := gcpfake.Error(http.StatusServiceUnavailable, "backendError", "The service is currently unavailable.")
		gcpfake.Start(t, &clientOptions).On("POST", path, outage, outage, outage, outage)
		_, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if err == nil || !strings.HasSuffix(err.Error(), "\nFix: Google returned a server error; run bqto again in a minute") {
			t.Errorf("err = %v, want the server-error fix", err)
		}
	})

	t.Run("a project the check cannot find is missing", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("POST", path, http.StatusNotFound, "notFound", "Project not found.")
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if warning != "" || err == nil || err.Error() != missingProject {
			t.Errorf("CheckIAPTunnel = %q, %v; want no warning and %q", warning, err, missingProject)
		}
	})

	t.Run("a denied impersonation names the service account", func(t *testing.T) {
		const sa = "bqto@p.iam.gserviceaccount.com"
		gcpfake.Start(t, &clientOptions)
		clientOptions = append(clientOptions, option.WithHTTPClient(&http.Client{Transport: failingToken{}}))
		warning, err := CheckIAPTunnel(context.Background(), "p", sa)
		want := "your account cannot impersonate " + sa + "\nFix: ask for roles/iam.serviceAccountTokenCreator on " + sa + " and wait a few minutes for the grant to apply, or stop impersonating: gcloud config unset auth/impersonate_service_account"
		if warning != "" || err == nil || err.Error() != want {
			t.Errorf("CheckIAPTunnel = %q, %v; want %q", warning, err, want)
		}
	})

	t.Run("an invalid project is missing", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("POST", path, http.StatusBadRequest, "badRequest", "Project specified in the request is invalid.")
		warning, err := CheckIAPTunnel(context.Background(), "p", "dev@example.com")
		if warning != "" || err == nil || err.Error() != missingProject {
			t.Errorf("CheckIAPTunnel = %q, %v; want no warning and %q", warning, err, missingProject)
		}
	})
}

type failingToken struct{}

func (failingToken) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New(`credentials: status code 403: {"error": {"code": 403, "message": "Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist).", "status": "PERMISSION_DENIED"}}`)
}

func noRetryWait(t *testing.T) {
	t.Helper()
	delays := gcperr.RetryDelays
	gcperr.RetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { gcperr.RetryDelays = delays })
}
