package quota

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/auth"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

type call struct {
	userProject string
	hasHeader   bool
	body        string
}

type fakeAPI struct {
	mu      sync.Mutex
	calls   []call
	respond func(userProject string) (int, string)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	up := r.Header.Get(Header)
	_, has := r.Header[http.CanonicalHeaderKey(Header)]
	f.mu.Lock()
	f.calls = append(f.calls, call{userProject: up, hasHeader: has, body: string(body)})
	f.mu.Unlock()
	status, out := http.StatusOK, `{"name":"ok"}`
	if f.respond != nil {
		status, out = f.respond(up)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, out)
}

func (f *fakeAPI) userProjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.userProject)
	}
	return out
}

func refusal(status int, reason, project string) (int, string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code":    status,
		"message": fmt.Sprintf("%s in project %s", reason, project),
		"details": []any{map[string]any{
			"@type":    "type.googleapis.com/google.rpc.ErrorInfo",
			"reason":   reason,
			"domain":   "googleapis.com",
			"metadata": map[string]any{"consumer": "projects/" + project, "service": "sqladmin.googleapis.com"},
		}},
	}})
	return status, string(b)
}

func adcOfType(t *testing.T, kind string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(f, []byte(`{"type":"`+kind+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", f)
	t.Setenv(EnvVar, "")
	refusedCalls.Clear()
	t.Cleanup(refusedCalls.Clear)
	listingRefusals.Clear()
	t.Cleanup(listingRefusals.Clear)
}

func clientFor(ctx context.Context, t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	base := []option.ClientOption{
		option.WithEndpoint(srv.URL),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})),
	}
	c, _, err := htransport.NewClient(ctx, Options(ctx, base...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func get(ctx context.Context, t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTheCallsProjectIsTheQuotaProject(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()

	for _, project := range []string{"prj-a", "prj-b"} {
		ctx := WithProject(context.Background(), project)
		if status, _ := get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x"); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
	}
	if got := strings.Join(api.userProjects(), ","); got != "prj-a,prj-b" {
		t.Errorf("quota projects sent = %q, want prj-a,prj-b", got)
	}
}

func TestTheLibraryDefaultStays(t *testing.T) {
	cases := []struct {
		desc    string
		adc     string
		env     string
		project string
	}{
		{desc: "no project for the call", adc: "authorized_user"},
		{desc: "GOOGLE_CLOUD_QUOTA_PROJECT wins", adc: "authorized_user", env: "prj-env", project: "prj-a"},
		{desc: "a service account key, as gcloud", adc: "service_account", project: "prj-a"},
		{desc: "an impersonated service account, as gcloud", adc: "impersonated_service_account", project: "prj-a"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			adcOfType(t, tc.adc)
			t.Setenv(EnvVar, tc.env)
			api := &fakeAPI{}
			srv := httptest.NewServer(api)
			defer srv.Close()
			ctx := WithProject(context.Background(), tc.project)
			get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
			if got := api.userProjects(); len(got) != 1 || got[0] != tc.env {
				t.Errorf("quota projects sent = %q, want only %q", got, tc.env)
			}
		})
	}
}

func TestNoADCFileIsNotAUser(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
	if UserCredentials() {
		t.Error("a missing ADC file counts as user credentials")
	}
}

func TestARefusingTargetFallsBackToTheADCQuotaProject(t *testing.T) {
	for _, reason := range []string{"USER_PROJECT_DENIED", "SERVICE_DISABLED", "CONSUMER_INVALID"} {
		t.Run(reason, func(t *testing.T) {
			adcOfType(t, "authorized_user")
			api := &fakeAPI{respond: func(up string) (int, string) {
				if up == "prj-target" {
					return refusal(http.StatusForbidden, reason, "prj-target")
				}
				return http.StatusOK, `{"name":"ok"}`
			}}
			srv := httptest.NewServer(api)
			defer srv.Close()
			ctx := WithProject(context.Background(), "prj-target")
			c := clientFor(ctx, t, srv)
			if status, body := get(ctx, t, c, srv.URL+"/v1/x"); status != http.StatusOK {
				t.Fatalf("first call = %d %s, want the fallback's 200", status, body)
			}
			if status, _ := get(ctx, t, c, srv.URL+"/v1/y"); status != http.StatusOK {
				t.Fatalf("second call = %d", status)
			}
			if got := strings.Join(api.userProjects(), ","); got != "prj-target,," {
				t.Errorf("quota projects sent = %q, want the target once, then the ADC default", got)
			}
		})
	}
}

func TestADeniedProjectIsSkippedByLaterClients(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(up string) (int, string) {
		if up == "prj-target" {
			return refusal(http.StatusForbidden, "USER_PROJECT_DENIED", "prj-target")
		}
		return http.StatusOK, `{}`
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-target")
	get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
	get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
	if got := strings.Join(api.userProjects(), ","); got != "prj-target,," {
		t.Errorf("quota projects sent = %q, want the target only once", got)
	}
}

func TestWhenBothRefuseTheTargetsErrorIsShown(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(up string) (int, string) {
		if up == "prj-target" {
			return refusal(http.StatusForbidden, "USER_PROJECT_DENIED", "prj-target")
		}
		return refusal(http.StatusForbidden, "SERVICE_DISABLED", "prj-adc-quota")
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-target")
	status, body := get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
	if status != http.StatusForbidden || !strings.Contains(body, "projects/prj-target") || strings.Contains(body, "prj-adc-quota") {
		t.Errorf("got %d %s, want the target's USER_PROJECT_DENIED", status, body)
	}
	if _, remembered := refusedCalls.Load("prj-target"); remembered {
		t.Error("a fallback that failed too is remembered")
	}
}

func TestOtherErrorsAreNotRetried(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(string) (int, string) {
		return refusal(http.StatusForbidden, "IAM_PERMISSION_DENIED", "prj-target")
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-target")
	status, body := get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
	if status != http.StatusForbidden || !strings.Contains(body, "IAM_PERMISSION_DENIED") {
		t.Errorf("got %d %s, want the IAM refusal", status, body)
	}
	if n := len(api.userProjects()); n != 1 {
		t.Errorf("%d calls, want 1", n)
	}
}

func TestAFallbackResendsTheBody(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(up string) (int, string) {
		if up == "prj-target" {
			return refusal(http.StatusForbidden, "USER_PROJECT_DENIED", "prj-target")
		}
		return http.StatusOK, `{}`
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-target")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/x", strings.NewReader(`{"vm":"bqto-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(ctx, t, srv).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.calls) != 2 || api.calls[1].body != `{"vm":"bqto-1"}` {
		t.Errorf("calls = %+v, want the body sent twice", api.calls)
	}
}

func TestADisabledAPIIsRememberedPerAPI(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(up string) (int, string) {
		if up == "prj-target" {
			return refusal(http.StatusForbidden, "SERVICE_DISABLED", "prj-target")
		}
		return http.StatusOK, `{}`
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-target")
	get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
	if _, whole := refusedCalls.Load("prj-target"); whole {
		t.Error("a disabled API marks the whole project as refused")
	}
	get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
	if got := strings.Join(api.userProjects(), ","); got != "prj-target,," {
		t.Errorf("quota projects sent = %q, want the target only once for this API", got)
	}
	otherAPI := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	get(ctx, t, clientFor(ctx, t, srv), otherAPI+"/v1/x")
	if got := api.userProjects()[3]; got != "prj-target" {
		t.Errorf("another API sent %q, want the target still tried", got)
	}
}

type staticToken struct{}

func (staticToken) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "fake", Type: "Bearer"}, nil
}

func listingClientFor(ctx context.Context, t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	base := []option.ClientOption{
		option.WithEndpoint(srv.URL),
		option.WithAuthCredentials(auth.NewCredentials(&auth.CredentialsOptions{
			TokenProvider: staticToken{},
			QuotaProjectIDProvider: auth.CredentialsPropertyFunc(func(context.Context) (string, error) {
				return cmp.Or(os.Getenv(EnvVar), "prj-adc-quota"), nil
			}),
		})),
	}
	c, _, err := htransport.NewClient(ctx, ListingOptions(ctx, base...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeAPI) userProjectHeaders() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bool
	for _, c := range f.calls {
		out = append(out, c.hasHeader)
	}
	return out
}

func TestAListingRefusedEverywhereIsSentWithNoQuotaProject(t *testing.T) {
	for _, reason := range []string{"SERVICE_DISABLED", "USER_PROJECT_DENIED", "CONSUMER_INVALID"} {
		t.Run(reason, func(t *testing.T) {
			adcOfType(t, "authorized_user")
			api := &fakeAPI{respond: func(up string) (int, string) {
				if up != "" {
					return refusal(http.StatusForbidden, reason, up)
				}
				return http.StatusOK, `{"projects":[]}`
			}}
			srv := httptest.NewServer(api)
			defer srv.Close()
			ctx := WithProject(context.Background(), "prj-gcloud")
			c := listingClientFor(ctx, t, srv)
			if status, body := get(ctx, t, c, srv.URL+"/v3/projects:search"); status != http.StatusOK {
				t.Fatalf("got %d %s, want the 200 sent with no quota project", status, body)
			}
			if got := strings.Join(api.userProjects(), ","); got != "prj-gcloud,prj-adc-quota," {
				t.Errorf("quota projects sent = %q, want the gcloud project, the ADC one, then none", got)
			}
			if got := api.userProjectHeaders(); len(got) != 3 || got[2] {
				t.Errorf("X-Goog-User-Project present per call = %v, want it absent on the last", got)
			}
			get(ctx, t, c, srv.URL+"/v3/folders:search")
			if got := api.userProjects()[3:]; len(got) != 1 || got[0] != "" {
				t.Errorf("a later call sent %q, want only the call with no quota project", got)
			}
		})
	}
}

func TestAListingWithNoGcloudProjectStartsAtTheADCQuotaProject(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(up string) (int, string) {
		if up != "" {
			return refusal(http.StatusForbidden, "SERVICE_DISABLED", up)
		}
		return http.StatusOK, `{}`
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := context.Background()
	if status, _ := get(ctx, t, listingClientFor(ctx, t, srv), srv.URL+"/v3/projects:search"); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := strings.Join(api.userProjects(), ","); got != "prj-adc-quota," {
		t.Errorf("quota projects sent = %q, want the ADC one, then none", got)
	}
}

func TestAListingThatWorksKeepsItsQuotaProject(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-gcloud")
	get(ctx, t, listingClientFor(ctx, t, srv), srv.URL+"/v3/projects:search")
	if got := strings.Join(api.userProjects(), ","); got != "prj-gcloud" {
		t.Errorf("quota projects sent = %q, want only the gcloud project", got)
	}
}

func TestAListingRefusedWithNoQuotaProjectShowsTheFirstError(t *testing.T) {
	adcOfType(t, "authorized_user")
	api := &fakeAPI{respond: func(up string) (int, string) {
		return refusal(http.StatusForbidden, "SERVICE_DISABLED", cmp.Or(up, "gcloud-client"))
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	ctx := WithProject(context.Background(), "prj-gcloud")
	status, body := get(ctx, t, listingClientFor(ctx, t, srv), srv.URL+"/v3/projects:search")
	if status != http.StatusForbidden || !strings.Contains(body, "projects/prj-gcloud") {
		t.Errorf("got %d %s, want the gcloud project's refusal", status, body)
	}
	if n := len(api.userProjects()); n != 3 {
		t.Errorf("%d calls, want 3", n)
	}
}

func TestAListingKeepsTheLibraryDefaultWhenTheUserDoesNotOwnTheQuota(t *testing.T) {
	cases := []struct {
		desc string
		adc  string
		env  string
		want string
	}{
		{desc: "GOOGLE_CLOUD_QUOTA_PROJECT is set", adc: "authorized_user", env: "prj-env", want: "prj-env"},
		{desc: "a service account", adc: "service_account", want: "prj-adc-quota"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			adcOfType(t, tc.adc)
			t.Setenv(EnvVar, tc.env)
			api := &fakeAPI{respond: func(up string) (int, string) {
				return refusal(http.StatusForbidden, "SERVICE_DISABLED", up)
			}}
			srv := httptest.NewServer(api)
			defer srv.Close()
			ctx := WithProject(context.Background(), "prj-gcloud")
			get(ctx, t, listingClientFor(ctx, t, srv), srv.URL+"/v3/projects:search")
			if got := api.userProjects(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("quota projects sent = %q, want only %q", got, tc.want)
			}
		})
	}
}

func TestAListingDoesNotChangeWhichProjectLaterCallsBlame(t *testing.T) {
	for _, reason := range []string{"SERVICE_DISABLED", "USER_PROJECT_DENIED"} {
		t.Run(reason, func(t *testing.T) {
			adcOfType(t, "authorized_user")
			api := &fakeAPI{respond: func(up string) (int, string) {
				if up != "" {
					return refusal(http.StatusForbidden, reason, up)
				}
				return http.StatusOK, `{}`
			}}
			srv := httptest.NewServer(api)
			defer srv.Close()
			ctx := WithProject(context.Background(), "prj-target")
			get(ctx, t, listingClientFor(ctx, t, srv), srv.URL+"/v3/projects:search")
			api.respond = func(up string) (int, string) {
				return refusal(http.StatusForbidden, reason, cmp.Or(up, "prj-adc-quota"))
			}
			before := len(api.userProjects())
			status, body := get(ctx, t, clientFor(ctx, t, srv), srv.URL+"/v1/x")
			if status != http.StatusForbidden || !strings.Contains(body, "projects/prj-target") {
				t.Errorf("got %d %s, want the call's project's refusal", status, body)
			}
			if got := api.userProjects()[before:]; len(got) != 2 || got[0] != "prj-target" {
				t.Errorf("quota projects sent = %q, want the call's project first, then the ADC one", got)
			}
		})
	}
}
