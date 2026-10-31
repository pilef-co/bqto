package gcperr

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClean(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"googleapi: Error 404: The Cloud SQL instance does not exist., instanceDoesNotExist", "The Cloud SQL instance does not exist"},
		{"googleapi: Error 403: Required 'compute.instances.create' permission, forbidden", "Required 'compute.instances.create' permission"},
		{"The Cloud SQL instance does not exist.", "The Cloud SQL instance does not exist"},
		{"plain message", "plain message"},
		{"a message, with a comma and more words", "a message, with a comma and more words"},
	}
	for _, c := range cases {
		if got := clean(c.in); got != c.want {
			t.Errorf("clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNotFound(t *testing.T) {
	cases := []struct {
		desc string
		err  error
		want bool
	}{
		{desc: "googleapi 404", err: &googleapi.Error{Code: 404, Message: "not found"}, want: true},
		{desc: "wrapped googleapi 404", err: fmt.Errorf("get instance: %w", &googleapi.Error{Code: 404}), want: true},
		{desc: "googleapi 403", err: &googleapi.Error{Code: 403}},
		{desc: "grpc NotFound", err: status.Error(codes.NotFound, "instance not found"), want: true},
		{desc: "grpc PermissionDenied", err: status.Error(codes.PermissionDenied, "nope")},
		{desc: "plain", err: errors.New("boom")},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := NotFound(c.err); got != c.want {
				t.Errorf("NotFound = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGet(t *testing.T) {
	missing := &googleapi.Error{Code: 404, Message: "The Cloud SQL instance does not exist."}
	got := Get("Cloud SQL instance", "bqto-test-sql", "p", "gcloud sql instances list --project p", missing).Error()
	want := "Cloud SQL instance bqto-test-sql not found in project p\nFix: gcloud sql instances list --project p"
	if got != want {
		t.Errorf("Get(404) = %q, want %q", got, want)
	}

	denied := &googleapi.Error{Code: 403, Message: "Required 'cloudsql.instances.get' permission."}
	if got := Get("Cloud SQL instance", "x", "p", "cmd", denied).Error(); got != "get Cloud SQL instance x in project p: Required 'cloudsql.instances.get' permission\nFix: ask for a role that grants cloudsql.instances.get; the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs" {
		t.Errorf("Get(403) = %q", got)
	}
}

func TestInvalidProject(t *testing.T) {
	invalid := &googleapi.Error{Code: 400, Message: "Project specified in the request is invalid.", Errors: []googleapi.ErrorItem{{Reason: "errorInvalidProject"}}}
	const missing = "project no-such-project does not exist, or your account cannot see it\nFix: check the ID with gcloud projects list; if the project exists but is not listed, ask for resourcemanager.projects.get on it (roles/browser)"
	if got := Get("Cloud SQL instance", "x", "no-such-project", "gcloud sql instances list --project no-such-project", invalid).Error(); got != missing {
		t.Errorf("Get(invalid project) = %q, want %q", got, missing)
	}
	if got := Wrap("list Cloud SQL instances", invalid).Error(); got != "list Cloud SQL instances: Project specified in the request is invalid\nFix: "+invalidProjectFix {
		t.Errorf("Wrap(invalid project) = %q", got)
	}
}

func TestProject(t *testing.T) {
	const missing = "project p does not exist, or your account cannot see it\nFix: check the ID with gcloud projects list; if the project exists but is not listed, ask for resourcemanager.projects.get on it (roles/browser)"
	tests := []struct {
		desc string
		err  error
		want string
	}{
		{desc: "not found", err: &googleapi.Error{Code: 404, Message: "Project not found."}, want: missing},
		{desc: "a denied read", err: &googleapi.Error{Code: 403, Message: "The caller does not have permission"}, want: missing},
		{desc: "a denied read that may not exist", err: &googleapi.Error{Code: 403, Message: "Permission 'resourcemanager.projects.get' denied on resource '//cloudresourcemanager.googleapis.com/projects/p' (or it may not exist)."}, want: missing},
		{desc: "an invalid project", err: &googleapi.Error{Code: 400, Message: "Project specified in the request is invalid."}, want: missing},
		{desc: "a disabled API is not a missing project", err: &googleapi.Error{Code: 403, Message: "Cloud Resource Manager API has not been used", Details: []any{map[string]any{
			"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "SERVICE_DISABLED",
			"metadata": map[string]any{"consumer": "projects/q", "service": "cloudresourcemanager.googleapis.com"},
		}}}, want: "read project p: Cloud Resource Manager API has not been used\nFix: gcloud services enable cloudresourcemanager.googleapis.com --project q"},
		{desc: "a denied read by IAM is a missing project", err: &googleapi.Error{Code: 403, Message: "The caller does not have permission", Details: []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "IAM_PERMISSION_DENIED"}}}, want: missing},
		{desc: "a token without the scope is not a missing project", err: &googleapi.Error{Code: 403, Message: "Request had insufficient authentication scopes.", Details: []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "ACCESS_TOKEN_SCOPE_INSUFFICIENT"}}},
			want: "read project p: Request had insufficient authentication scopes\nFix: " + genericForbidden},
		{desc: "an outage is not a missing project", err: &googleapi.Error{Code: 503, Message: "Unavailable."}, want: "read project p: Unavailable\nFix: " + serverFix},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := Project("read project p", "p", tc.err).Error(); got != tc.want {
				t.Errorf("Project = %q\nwant      %q", got, tc.want)
			}
		})
	}
	if Project("op", "p", nil) != nil {
		t.Error("Project(nil) is not nil")
	}
}

const deniedImpersonation = `credentials: status code 403: {
  "error": {
    "code": 403,
    "message": "Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist).",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "IAM_PERMISSION_DENIED",
        "domain": "iam.googleapis.com",
        "metadata": {
          "permission": "iam.serviceAccounts.getAccessToken"
        }
      }
    ]
  }
}`

func TestImpersonationDenied(t *testing.T) {
	const want = "check the IAP tunnel permission on project p: your account cannot impersonate the service account it is set to impersonate\nFix: ask for roles/iam.serviceAccountTokenCreator on the service account it is set to impersonate and wait a few minutes for the grant to apply, or stop impersonating: gcloud config unset auth/impersonate_service_account"
	tests := []struct {
		desc string
		err  error
	}{
		{desc: "over HTTP", err: &url.Error{Op: "Post", URL: "https://cloudresourcemanager.googleapis.com/v3/projects/p:testIamPermissions?alt=json&prettyPrint=false", Err: errors.New(deniedImpersonation)}},
		{desc: "over gRPC", err: status.Error(codes.Unauthenticated, "transport: per-RPC creds failed due to error: "+deniedImpersonation)},
		{desc: "from the impersonate package", err: &url.Error{Op: "Get", URL: "https://compute.googleapis.com", Err: errors.New(`impersonate: status code 403: {"error": {"code": 403, "message": "denied"}}`)}},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := Wrap("check the IAP tunnel permission on project p", tc.err).Error(); got != want {
				t.Errorf("Wrap = %q\nwant   %q", got, want)
			}
		})
	}
	if ImpersonationDenied(&url.Error{Op: "Post", URL: "https://sts.googleapis.com", Err: errors.New(`credentials: status code 403: {"error": "access_denied"}`)}) {
		t.Error("a 403 from the token exchange is not an impersonation")
	}
	if ImpersonationDenied(&googleapi.Error{Code: 403, Message: "Permission 'iam.serviceAccounts.getAccessToken' denied"}) {
		t.Error("an API answer is not a failed token fetch")
	}
	if got := CannotImpersonate("sa@p.iam.gserviceaccount.com").Error(); !strings.HasPrefix(got, "your account cannot impersonate sa@p.iam.gserviceaccount.com\nFix: ask for roles/iam.serviceAccountTokenCreator on sa@p.iam.gserviceaccount.com") {
		t.Errorf("CannotImpersonate = %q", got)
	}
}

func TestInfo(t *testing.T) {
	info := map[string]any{
		"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
		"reason": "SERVICE_DISABLED",
		"metadata": map[string]any{
			"consumer": "projects/quota-p",
			"service":  "cloudresourcemanager.googleapis.com",
		},
	}
	help := map[string]any{"@type": "type.googleapis.com/google.rpc.Help"}
	tests := []struct {
		desc       string
		details    []any
		wantReason string
		wantMeta   map[string]string
	}{
		{desc: "error info after other details", details: []any{help, info}, wantReason: "SERVICE_DISABLED", wantMeta: map[string]string{"consumer": "projects/quota-p", "service": "cloudresourcemanager.googleapis.com"}},
		{desc: "no error info", details: []any{help}},
		{desc: "no details"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			reason, meta := Info(&googleapi.Error{Code: 403, Details: tc.details})
			if reason != tc.wantReason || !maps.Equal(meta, tc.wantMeta) {
				t.Errorf("Info() = %q, %v; want %q, %v", reason, meta, tc.wantReason, tc.wantMeta)
			}
		})
	}
}

func TestSplitFix(t *testing.T) {
	msg, fix := SplitFix(errors.New("boom\nFix: run it"))
	if msg != "boom" || fix != "run it" {
		t.Errorf("SplitFix = %q, %q", msg, fix)
	}
	if msg, fix := SplitFix(errors.New("boom")); msg != "boom" || fix != "" {
		t.Errorf("SplitFix with no fix = %q, %q", msg, fix)
	}
}

func TestWrap(t *testing.T) {
	disabled := &googleapi.Error{Code: 403, Message: "Cloud Memorystore for Redis API has not been used in project p before or it is disabled.", Details: []any{map[string]any{
		"@type":    "type.googleapis.com/google.rpc.ErrorInfo",
		"reason":   "SERVICE_DISABLED",
		"metadata": map[string]any{"consumer": "projects/p", "service": "redis.googleapis.com"},
	}}}
	tests := []struct {
		desc, op string
		err      error
		want     string
	}{
		{desc: "a missing permission", op: "list Cloud SQL instances in project p", err: &googleapi.Error{Code: 403, Message: "The client is not authorized to make this request., notAuthorized"},
			want: "list Cloud SQL instances in project p: The client is not authorized to make this request\nFix: check that your account has the roles bqto needs, and that the project and resource names are right; the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs"},
		{desc: "a named permission", op: "create bastion", err: &googleapi.Error{Code: 403, Message: "Required 'compute.instances.create' permission for 'projects/p/zones/z/instances/bqto-1', forbidden"},
			want: "create bastion: Required 'compute.instances.create' permission for 'projects/p/zones/z/instances/bqto-1'\nFix: ask for a role that grants compute.instances.create; the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs"},
		{desc: "a grpc permission", op: "list Valkey instances", err: status.Error(codes.PermissionDenied, "Permission 'memorystore.instances.list' denied on resource"),
			want: "list Valkey instances: Permission 'memorystore.instances.list' denied on resource\nFix: ask for a role that grants memorystore.instances.list; the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs"},
		{desc: "a disabled API", op: "list Redis instances", err: disabled,
			want: "list Redis instances: Cloud Memorystore for Redis API has not been used in project p before or it is disabled\nFix: gcloud services enable redis.googleapis.com --project p"},
		{desc: "no credentials", op: "list", err: &googleapi.Error{Code: 401, Message: "Request had invalid authentication credentials."},
			want: "list: Request had invalid authentication credentials\nFix: gcloud auth application-default login"},
		{desc: "a timeout", op: "get subnet", err: context.DeadlineExceeded,
			want: "get subnet: context deadline exceeded\nFix: the call timed out; check your network and run bqto again"},
		{desc: "VPC Service Controls", op: "list", err: &googleapi.Error{Code: 403, Message: "Request is prohibited by organization's policy. vpcServiceControlsUniqueIdentifier: abc"},
			want: "list: Request is prohibited by organization's policy. vpcServiceControlsUniqueIdentifier: abc\nFix: a VPC Service Controls perimeter blocked the call; ask the perimeter's admin for an ingress rule for your identity"},
		{desc: "VPC Service Controls by reason", op: "list", err: &googleapi.Error{Code: 403, Message: "Request is prohibited by organization's policy.", Details: []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "SECURITY_POLICY_VIOLATED"}}},
			want: "list: Request is prohibited by organization's policy\nFix: a VPC Service Controls perimeter blocked the call; ask the perimeter's admin for an ingress rule for your identity"},
		{desc: "billing disabled", op: "list", err: &googleapi.Error{Code: 403, Message: "This API method requires billing to be enabled.", Details: []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "BILLING_DISABLED"}}},
			want: "list: This API method requires billing to be enabled\nFix: enable billing on the project, or ask its owner to"},
		{desc: "a permission that mentions billing is not a billing problem", op: "get", err: &googleapi.Error{Code: 403, Message: "Required 'billing.accounts.get' permission"},
			want: "get: Required 'billing.accounts.get' permission\nFix: ask for a role that grants billing.accounts.get; the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs"},
		{desc: "a mistyped name", op: "get image p/x", err: &googleapi.Error{Code: 404, Message: "The resource 'projects/p/global/images/x' was not found, notFound"},
			want: "get image p/x: The resource 'projects/p/global/images/x' was not found\nFix: check the name and the project: it may be mistyped or deleted"},
		{desc: "a server error", op: "get subnet", err: &googleapi.Error{Code: 500, Message: "Internal error."}, want: "get subnet: Internal error\nFix: " + serverFix},
		{desc: "a 503", op: "list", err: &googleapi.Error{Code: 503, Message: "The service is currently unavailable."}, want: "list: The service is currently unavailable\nFix: " + serverFix},
		{desc: "a grpc Unavailable", op: "list Valkey instances", err: status.Error(codes.Unavailable, "the service is currently unavailable"), want: "list Valkey instances: the service is currently unavailable\nFix: " + serverFix},
		{desc: "a DNS failure", op: "list", err: &url.Error{Op: "Get", URL: "https://compute.googleapis.com", Err: &net.DNSError{Err: "no such host", Name: "compute.googleapis.com"}},
			want: "list: Get \"https://compute.googleapis.com\": lookup compute.googleapis.com: no such host\nFix: " + networkFix},
		{desc: "a refused connection", op: "list", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			want: "list: dial tcp: connection refused\nFix: " + networkFix},
		{desc: "a reauth request is not the network", op: "list AlloyDB instances", err: &url.Error{Op: "Get", URL: "https://alloydb.googleapis.com", Err: errors.New(`auth: "invalid_grant" "reauth related error (invalid_rapt)" "https://support.google.com/a/answer/9368756"`)},
			want: "list AlloyDB instances: Get \"https://alloydb.googleapis.com\": auth: \"invalid_grant\" \"reauth related error (invalid_rapt)\" \"https://support.google.com/a/answer/9368756\"\nFix: Google asks you to sign in again: gcloud auth login, then gcloud auth application-default login"},
		{desc: "a reauth request over gRPC", op: "get cluster", err: status.Error(codes.Unauthenticated, `transport: per-RPC creds failed due to error: auth: "invalid_grant" "reauth related error (invalid_rapt)"`),
			want: "get cluster: transport: per-RPC creds failed due to error: auth: \"invalid_grant\" \"reauth related error (invalid_rapt)\"\nFix: Google asks you to sign in again: gcloud auth login, then gcloud auth application-default login"},
		{desc: "a revoked refresh token is an expired login", op: "get vm", err: &url.Error{Op: "Get", URL: "https://compute.googleapis.com", Err: errors.New(`auth: "invalid_grant" "Token has been expired or revoked."`)},
			want: "get vm: Get \"https://compute.googleapis.com\": auth: \"invalid_grant\" \"Token has been expired or revoked.\"\nFix: your login expired: gcloud auth application-default login"},
		{desc: "an expired login is not the network", op: "get vm", err: &url.Error{Op: "Get", URL: "https://compute.googleapis.com", Err: errors.New("oauth2: cannot fetch token: 400 Bad Request, invalid_grant")},
			want: "get vm: Get \"https://compute.googleapis.com\": oauth2: cannot fetch token: 400 Bad Request\nFix: your login expired: gcloud auth application-default login"},
		{desc: "no fix to give", op: "get subnet", err: &googleapi.Error{Code: 400, Message: "Bad request."}, want: "get subnet: Bad request"},
		{desc: "an inner error with its own fix", op: "get vm", err: errors.New("the token expired\nFix: gcloud auth login"), want: "get vm: the token expired\nFix: gcloud auth login"},
		{desc: "a quota is a 403 but not a role", op: "create bastion", err: &googleapi.Error{Code: 403, Message: "Quota 'CPUS' exceeded. Limit: 8.0 in region us-central1., quotaExceeded"},
			want: "create bastion: Quota 'CPUS' exceeded. Limit: 8.0 in region us-central1\nFix: " + quotaFix},
		{desc: "a quota by reason", op: "list", err: &googleapi.Error{Code: 403, Message: "Limit exceeded", Errors: []googleapi.ErrorItem{{Reason: "quotaExceeded"}}},
			want: "list: Limit exceeded\nFix: " + quotaFix},
		{desc: "a rate limit is a 403 but not a role", op: "list", err: &googleapi.Error{Code: 403, Message: "User Rate Limit Exceeded", Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}},
			want: "list: User Rate Limit Exceeded\nFix: " + rateFix},
		{desc: "a 429", op: "list", err: &googleapi.Error{Code: 429, Message: "Too many requests"},
			want: "list: Too many requests\nFix: " + rateFix},
		{desc: "a per-minute quota is a wait", op: "list", err: status.Error(codes.ResourceExhausted, "Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute'"),
			want: "list: Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute'\nFix: " + rateFix},
		{desc: "a daily quota on a 429 is not a wait", op: "list", err: &googleapi.Error{Code: 429, Message: "Daily Limit Exceeded", Errors: []googleapi.ErrorItem{{Reason: "dailyLimitExceeded"}}},
			want: "list: Daily Limit Exceeded\nFix: " + quotaFix},
		{desc: "ResourceExhausted that is not a quota gets no fix", op: "list", err: status.Error(codes.ResourceExhausted, "grpc: received message larger than max (5000000 vs. 4194304)"),
			want: "list: grpc: received message larger than max (5000000 vs. 4194304)"},
		{desc: "a denied permission on a project that may not exist", op: "list", err: status.Error(codes.PermissionDenied, "Permission 'memorystore.instances.list' denied on resource '//memorystore.googleapis.com/projects/typo/locations/-' (or it may not exist)"),
			want: "list: Permission 'memorystore.instances.list' denied on resource '//memorystore.googleapis.com/projects/typo/locations/-' (or it may not exist)\nFix: check the project and resource name, which may not exist; if they are right, ask for a role that grants memorystore.instances.list; the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := Wrap(tc.op, tc.err)
			if err.Error() != tc.want {
				t.Errorf("Wrap = %q\nwant   %q", err.Error(), tc.want)
			}
			if !errors.Is(err, tc.err) {
				t.Error("the cause is lost: errors.Is fails")
			}
		})
	}
	if _, ok := errors.AsType[*googleapi.Error](Wrap("op", disabled)); !ok {
		t.Error("errors.As cannot reach the googleapi.Error")
	}
	if Wrap("op", nil) != nil {
		t.Error("Wrap(nil) is not nil")
	}
	twice := Wrap("outer", Wrap("inner", disabled))
	if strings.Count(twice.Error(), "Fix:") != 1 || !strings.HasPrefix(twice.Error(), "inner") {
		t.Errorf("wrapping twice = %q, want the inner message once", twice)
	}
}

func TestWrapForbidden(t *testing.T) {
	const fix = "ask for resourcemanager.projects.get on project 123 (roles/browser)"
	plain := &googleapi.Error{Code: 403, Message: "The caller does not have permission"}
	if got := WrapForbidden("read project 123", plain, fix).Error(); !strings.HasSuffix(got, "\nFix: "+fix) {
		t.Errorf("a plain 403 = %q, want the given fix", got)
	}
	disabled := &googleapi.Error{Code: 403, Message: "Cloud Resource Manager API has not been used", Details: []any{map[string]any{
		"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "SERVICE_DISABLED",
		"metadata": map[string]any{"consumer": "projects/p", "service": "cloudresourcemanager.googleapis.com"},
	}}}
	if got := WrapForbidden("read project 123", disabled, fix).Error(); !strings.HasSuffix(got, "\nFix: gcloud services enable cloudresourcemanager.googleapis.com --project p") {
		t.Errorf("a disabled API = %q, want the enable command", got)
	}
	billing := &googleapi.Error{Code: 403, Message: "The billing account for the owning project is disabled in state closed"}
	if got := WrapForbidden("read project 123", billing, fix).Error(); !strings.HasSuffix(got, "\nFix: enable billing on the project, or ask its owner to") {
		t.Errorf("a billing 403 = %q, want the billing fix", got)
	}
	if got := WrapForbidden("read project 123", &googleapi.Error{Code: 404, Message: "not found"}, fix).Error(); strings.Contains(got, fix) {
		t.Errorf("a 404 = %q, want the 404 fix", got)
	}
}

func TestBillingWordings(t *testing.T) {
	for _, msg := range []string{"The billing account for the owning project is disabled in state closed", "Billing must be enabled for activation of service(s)"} {
		if got := Wrap("op", &googleapi.Error{Code: 403, Message: msg}).Error(); !strings.HasSuffix(got, "\nFix: enable billing on the project, or ask its owner to") {
			t.Errorf("%q = %q, want the billing fix", msg, got)
		}
	}
}

func TestUserProjectDeniedNamesTheProject(t *testing.T) {
	denied := func(metadata map[string]any) error {
		return &googleapi.Error{Code: 403, Message: "Caller does not have required permission to use project prj-app.", Details: []any{map[string]any{
			"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "USER_PROJECT_DENIED", "metadata": metadata,
		}}}
	}
	tests := []struct {
		desc string
		err  error
		want string
	}{
		{desc: "with the consumer", err: denied(map[string]any{"consumer": "projects/prj-app"}),
			want: "Fix: your account cannot use project prj-app for API checks and quota: ask for roles/serviceusage.serviceUsageConsumer on it, or export GOOGLE_CLOUD_QUOTA_PROJECT=PROJECT, a project where you have that role and the API is enabled"},
		{desc: "without the consumer", err: denied(nil),
			want: "Fix: your account cannot use the quota project for API checks and quota: ask for roles/serviceusage.serviceUsageConsumer on it"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := Wrap("get Cloud SQL instance sql in project prj-app", tc.err).Error(); !strings.Contains(got, tc.want) {
				t.Errorf("Wrap = %q\nwant it to contain %q", got, tc.want)
			}
		})
	}
}
