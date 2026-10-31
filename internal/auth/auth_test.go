package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareAccounts(t *testing.T) {
	const sa, sa2 = "bqto@p.iam.gserviceaccount.com", "final@p.iam.gserviceaccount.com"
	tests := []struct {
		adc, key, gcloud, impersonated string
		wantErr                        bool
		wantFix                        string
	}{
		{adc: "user@example.com", gcloud: "user@example.com"},
		{adc: "USER@EXAMPLE.COM", gcloud: "user@example.com"},
		{adc: "user@example.com", gcloud: "USER@EXAMPLE.COM"},
		{adc: "user@example.com", gcloud: "other@example.com", wantErr: true, wantFix: "gcloud auth application-default login"},
		{adc: "user@example.com", gcloud: "", wantErr: true, wantFix: "gcloud auth login"},
		{adc: "", gcloud: "user@example.com", wantErr: true, wantFix: "gcloud auth application-default login"},
		{adc: sa, gcloud: "user@example.com", impersonated: sa},
		{adc: sa2, gcloud: "user@example.com", impersonated: sa + "," + sa2},
		{adc: "user@example.com", gcloud: "user@example.com", impersonated: sa, wantErr: true, wantFix: "gcloud config unset auth/impersonate_service_account"},
		{adc: "user@example.com", gcloud: "user@example.com", impersonated: sa, wantErr: true, wantFix: "--impersonate-service-account " + sa},
		{adc: sa, gcloud: "user@example.com", wantErr: true, wantFix: "gcloud config set auth/impersonate_service_account " + sa},
		{adc: sa, gcloud: "user@example.com", wantErr: true, wantFix: "not only bqto"},
		{adc: sa, key: "/k.json", gcloud: "user@example.com", wantErr: true, wantFix: "gcloud auth activate-service-account --key-file=/k.json"},
	}
	for _, tc := range tests {
		err := compareAccounts(tc.adc, tc.key, tc.gcloud, tc.impersonated)
		if (err != nil) != tc.wantErr {
			t.Errorf("compareAccounts(%q, %q, %q, %q) error = %v, wantErr %v", tc.adc, tc.key, tc.gcloud, tc.impersonated, err, tc.wantErr)
			continue
		}
		if tc.wantErr && !strings.Contains(err.Error(), tc.wantFix) {
			t.Errorf("compareAccounts(%q, %q, %q, %q) error = %q, want it to contain %q", tc.adc, tc.key, tc.gcloud, tc.impersonated, err, tc.wantFix)
		}
	}
}

func TestMissingIAPWarning(t *testing.T) {
	tests := []struct {
		desc    string
		email   string
		granted []string
		wantFix string
	}{
		{desc: "granted", email: "dev@example.com", granted: []string{"iap.tunnelInstances.accessViaIAP"}},
		{desc: "missing, user", email: "dev@example.com", wantFix: "--member=user:dev@example.com --role=roles/iap.tunnelResourceAccessor"},
		{desc: "missing, service account", email: "ci@p.iam.gserviceaccount.com", granted: []string{"compute.instances.create"}, wantFix: "--member=serviceAccount:ci@p.iam.gserviceaccount.com"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := missingIAPWarning("p", tc.email, tc.granted)
			if tc.wantFix == "" {
				if got != "" {
					t.Errorf("warning = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantFix) {
				t.Errorf("warning = %q, want it to contain %q", got, tc.wantFix)
			}
		})
	}
}

func TestIAPCheckError(t *testing.T) {
	tests := []struct {
		desc     string
		reason   string
		metadata map[string]string
		want     string
	}{
		{
			desc:   "api disabled",
			reason: "SERVICE_DISABLED",
			metadata: map[string]string{
				"consumer":     "projects/quota-p",
				"service":      "cloudresourcemanager.googleapis.com",
				"serviceTitle": "Cloud Resource Manager API",
			},
			want: "cannot check the IAP tunnel permission: bqto needs the Cloud Resource Manager API, which is disabled in project quota-p\nFix: gcloud services enable cloudresourcemanager.googleapis.com --project quota-p",
		},
		{desc: "another error is left to gcperr", reason: "RATE_LIMIT_EXCEEDED"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := iapCheckError(tc.reason, tc.metadata)
			if tc.want == "" {
				if err != nil {
					t.Errorf("iapCheckError() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Errorf("iapCheckError() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestADCErrorKeepsTheCause(t *testing.T) {
	cause := errors.New("open /nonexistent.json: no such file or directory")
	got := adcError(cause, "/nonexistent.json").Error()
	for _, want := range []string{"GOOGLE_APPLICATION_CREDENTIALS=/nonexistent.json", "no such file", "Fix: point the variable at a valid key file, or unset it"} {
		if !strings.Contains(got, want) {
			t.Errorf("adcError = %q, want it to contain %q", got, want)
		}
	}
	if got := adcError(cause, "").Error(); !strings.Contains(got, "Fix: gcloud auth application-default login") || !strings.Contains(got, "no such file") {
		t.Errorf("adcError with no variable = %q", got)
	}
}

func TestADCErrorWithNoCredentials(t *testing.T) {
	cause := errors.New("google: could not find default credentials. See https://cloud.google.com/docs/authentication/external/set-up-adc for more information")
	const want = "no Application Default Credentials found\nFix: gcloud auth application-default login"
	if got := adcError(cause, "").Error(); got != want {
		t.Errorf("adcError = %q, want %q", got, want)
	}
}

func TestWithStderr(t *testing.T) {
	_, err := exec.Command("sh", "-c", "echo 'ERROR: (gcloud.config.get-value) bad property' >&2; exit 1").Output()
	got := WithStderr(err)
	if !strings.Contains(got.Error(), "exit status 1: ERROR: (gcloud.config.get-value) bad property") {
		t.Errorf("WithStderr = %q, want the exit status and the stderr", got)
	}
	if _, ok := errors.AsType[*exec.ExitError](got); !ok {
		t.Error("the ExitError is lost")
	}
}

func TestImpersonatedEmail(t *testing.T) {
	tests := []struct {
		desc string
		json string
		want string
	}{
		{
			desc: "impersonated ADC",
			json: `{"type":"impersonated_service_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/bqto@p.iam.gserviceaccount.com:generateAccessToken"}`,
			want: "bqto@p.iam.gserviceaccount.com",
		},
		{
			desc: "Workload Identity Federation, as GitHub Actions gets it",
			json: `{"type":"external_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/ci%40p.iam.gserviceaccount.com:generateAccessToken"}`,
			want: "ci@p.iam.gserviceaccount.com",
		},
		{desc: "a user's ADC", json: `{"type":"authorized_user","client_id":"x"}`},
		{desc: "not JSON", json: `nope`},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := impersonatedEmail([]byte(tc.json)); got != tc.want {
				t.Errorf("impersonatedEmail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyFile(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	tests := []struct {
		desc, json, env, want string
	}{
		{desc: "a key file named by the variable", json: `{"type":"service_account"}`, env: "/k.json", want: "/k.json"},
		{desc: "a key file found elsewhere", json: `{"type":"service_account"}`, want: "KEY_FILE"},
		{desc: "user credentials", json: `{"type":"authorized_user"}`, env: "/k.json"},
		{desc: "impersonated credentials", json: `{"type":"impersonated_service_account"}`},
		{desc: "workload identity federation", json: `{"type":"external_account"}`},
		{desc: "not JSON", json: `x`},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", tc.env)
			if got := keyFile([]byte(tc.json)); got != tc.want {
				t.Errorf("keyFile = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestADCFixForANetworkFailure(t *testing.T) {
	login := "gcloud auth application-default login"
	cases := []struct {
		err  error
		want bool
	}{
		{&url.Error{Op: "Post", URL: "https://oauth2.googleapis.com/tokeninfo", Err: errors.New("proxyconnect tcp: connection refused")}, false},
		{unreachable{errors.New("tokeninfo: HTTP 503")}, false},
		{errors.New("tokeninfo: Invalid Value"), true},
	}
	for _, c := range cases {
		if got := adcFix(c.err) == login; got != c.want {
			t.Errorf("adcFix(%v) = %q, want login advice: %v", c.err, adcFix(c.err), c.want)
		}
	}
}

func TestGcloudVersionError(t *testing.T) {
	cases := []struct {
		out     string
		wantErr bool
	}{
		{`{"Google Cloud SDK": "540.0.0", "core": "2025.09.23"}`, true},
		{`{"Google Cloud SDK": "549.0.0"}`, true},
		{`{"Google Cloud SDK": "540.0.0", "bq": null, "components": {"kubectl": 1}}`, true},
		{`{"Google Cloud SDK": "550.0.0"}`, false},
		{`{"Google Cloud SDK": "585.0.0"}`, false},
		{`{"core": "2026.01.01"}`, false},
		{`not json`, false},
	}
	for _, c := range cases {
		err := gcloudVersionError([]byte(c.out), 550, "update it")
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.out, err, c.wantErr)
		}
		if err != nil && !strings.HasSuffix(err.Error(), "\nFix: update it") {
			t.Errorf("%s: err = %q, want the Fix last", c.out, err)
		}
	}
}

func TestADCFixNamesTheCACertificatesWhenTLSIsNotTrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	resp, fromTLS := http.Get(srv.URL) //nolint:noctx // a local test server
	if fromTLS == nil {
		_ = resp.Body.Close()
		t.Fatal("the default client trusted a self-signed test server")
	}
	impersonated := errors.New("oauth2/google: unable to generate access token: " + fromTLS.Error())
	for _, err := range []error{fromTLS, impersonated} {
		if got := adcFix(err); !strings.Contains(got, "ca-certificates") {
			t.Errorf("adcFix(%v) = %q; want the CA certificates fix", err, got)
		}
	}
}

func TestCheckNamesAMissingSSHClient(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, _, err := Check(t.Context())
	if err == nil || !strings.Contains(err.Error(), "ssh not found") || !strings.Contains(err.Error(), "openssh-client") {
		t.Errorf("Check with no ssh on PATH: %v", err)
	}
}
