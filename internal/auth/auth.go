package auth

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/oauth2/google"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/quota"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
)

var clientOptions []option.ClientOption

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

func Check(ctx context.Context) (email, impersonated string, err error) {
	if _, err := exec.LookPath("gcloud"); err != nil {
		return "", "", fmt.Errorf("gcloud not found on PATH\nFix: install the Google Cloud SDK: https://cloud.google.com/sdk/docs/install")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return "", "", errors.New("ssh not found on PATH, and gcloud needs it for the tunnel\nFix: install an SSH client (on Debian or Ubuntu: apt-get install openssh-client)")
	}

	creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return "", "", adcError(err, os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
	}

	adcEmail, err := resolveADCEmail(ctx, creds)
	if err != nil {
		return "", "", err
	}

	gcloudAcct, err := gcloudConfig("account")
	if err != nil {
		return "", "", fmt.Errorf("cannot read the active gcloud account: %w\nFix: gcloud auth login", err)
	}
	impersonated, err = gcloudConfig("auth/impersonate_service_account")
	if err != nil {
		return "", "", fmt.Errorf("cannot read the service account gcloud impersonates: %w\nFix: check that gcloud config get-value auth/impersonate_service_account works", err)
	}
	if err := compareAccounts(adcEmail, keyFile(creds.JSON), gcloudAcct, impersonated); err != nil {
		return "", "", err
	}
	return adcEmail, impersonated, nil
}

func DefaultProject() string {
	p, _ := gcloudConfig("project")
	return p
}

func resolveADCEmail(ctx context.Context, creds *google.Credentials) (string, error) {
	var sa struct {
		ClientEmail string `json:"client_email"`
	}
	if json.Unmarshal(creds.JSON, &sa) == nil && sa.ClientEmail != "" {
		return sa.ClientEmail, nil
	}
	if email := impersonatedEmail(creds.JSON); email != "" {
		return email, nil
	}

	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("cannot obtain an ADC token: %w\nFix: %s", err, adcFix(err))
	}
	email, err := tokenEmail(ctx, token.AccessToken)
	if err != nil {
		return "", fmt.Errorf("cannot read the email of your ADC login: %w\nFix: %s", err, adcFix(err))
	}
	return email, nil
}

var impersonationURL = regexp.MustCompile(`/serviceAccounts/([^/:]+):generateAccessToken`)

func impersonatedEmail(credsJSON []byte) string {
	var c struct {
		URL string `json:"service_account_impersonation_url"`
	}
	if json.Unmarshal(credsJSON, &c) != nil {
		return ""
	}
	m := impersonationURL.FindStringSubmatch(c.URL)
	if m == nil {
		return ""
	}
	email, err := url.PathUnescape(m[1])
	if err != nil {
		return ""
	}
	return email
}

func keyFile(credsJSON []byte) string {
	var c struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(credsJSON, &c) != nil || c.Type != "service_account" {
		return ""
	}
	return cmp.Or(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"), "KEY_FILE")
}

type unreachable struct{ error }

func (u unreachable) Unwrap() error { return u.error }

func adcFix(err error) string {
	if errors.As(err, new(x509.UnknownAuthorityError)) || strings.Contains(err.Error(), "certificate signed by unknown authority") {
		return "this machine has no CA certificates to check Google's TLS certificate; install them (on Debian or Ubuntu: apt-get install ca-certificates), or behind a TLS-inspecting proxy add its CA (SSL_CERT_FILE on Linux, the Keychain on macOS)"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) || errors.As(err, new(unreachable)) {
		return "Google could not be reached; check your network or proxy and run bqto again"
	}
	return "gcloud auth application-default login"
}

func tokenEmail(ctx context.Context, accessToken string) (string, error) {

	formBody := strings.NewReader("access_token=" + url.QueryEscape(accessToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/tokeninfo", formBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tokeninfo request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.ErrorDescription != "" {
			return "", fmt.Errorf("tokeninfo: %s", errResp.ErrorDescription)
		}
		err := fmt.Errorf("tokeninfo: HTTP %d", resp.StatusCode)
		if resp.StatusCode >= http.StatusInternalServerError {
			return "", unreachable{err}
		}
		return "", err
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("tokeninfo parse: %w", err)
	}
	if info.Email == "" {
		return "", fmt.Errorf("tokeninfo returned no email field")
	}
	return info.Email, nil
}

func adcError(err error, keyFile string) error {
	if keyFile != "" {
		return fmt.Errorf("cannot load Application Default Credentials from GOOGLE_APPLICATION_CREDENTIALS=%s: %w\nFix: point the variable at a valid key file, or unset it and run gcloud auth application-default login", keyFile, err)
	}
	if strings.Contains(err.Error(), "could not find default credentials") {
		return errors.New("no Application Default Credentials found\nFix: gcloud auth application-default login")
	}
	return fmt.Errorf("cannot load Application Default Credentials: %w\nFix: gcloud auth application-default login", err)
}

func CheckGcloudVersion(ctx context.Context, minMajor int, updateFix string) error {
	ext.Debugf("gcloud: gcloud version --format=json")
	out, err := exec.CommandContext(ctx, "gcloud", "version", "--format=json").Output()
	if err != nil {
		return nil //nolint:nilerr // with no gcloud version to read there is nothing to check; the gcloud calls that follow report the problem
	}
	return gcloudVersionError(out, minMajor, updateFix)
}

func gcloudVersionError(versionJSON []byte, minMajor int, updateFix string) error {
	var v struct {
		SDK string `json:"Google Cloud SDK"`
	}
	if json.Unmarshal(versionJSON, &v) != nil {
		return nil //nolint:nilerr // an unreadable version is not a reason to stop the session
	}
	sdk := v.SDK
	major, err := strconv.Atoi(strings.SplitN(sdk, ".", 2)[0])
	if err != nil || major >= minMajor {
		return nil //nolint:nilerr // an unreadable version is not a reason to stop the session
	}
	return fmt.Errorf("gcloud %s is too old: bqto's bastions use OS Login certificates, which gcloud compute ssh supports from %d.0.0\nFix: %s", sdk, minMajor, updateFix)
}

func gcloudConfig(key string) (string, error) {
	ext.Debugf("gcloud: gcloud config get-value %s", key)
	out, err := exec.Command("gcloud", "config", "get-value", key).Output() //nolint:gosec // key is one of bqto's own gcloud property names
	if err != nil {
		return "", WithStderr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

func WithStderr(err error) error {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}

func compareAccounts(adcEmail, adcKeyFile, gcloudAccount, impersonated string) error {
	if chain := strings.Split(impersonated, ","); impersonated != "" {
		acting := strings.TrimSpace(chain[len(chain)-1])
		if strings.EqualFold(adcEmail, acting) {
			return nil
		}
		return fmt.Errorf(
			"ADC account (%s) does not match the account gcloud impersonates (%s)\n"+
				"Fix: gcloud auth application-default login --impersonate-service-account %s, or stop impersonating in gcloud with gcloud config unset auth/impersonate_service_account",
			adcEmail, acting, impersonated,
		)
	}
	if strings.EqualFold(adcEmail, gcloudAccount) {
		return nil
	}
	fixGcloud := "gcloud auth login"
	switch {
	case adcKeyFile != "":
		fixGcloud = "gcloud auth activate-service-account --key-file=" + adcKeyFile
	case strings.HasSuffix(adcEmail, ".gserviceaccount.com"):
		fixGcloud = "gcloud config set auth/impersonate_service_account " + adcEmail + " (this applies to every gcloud command, not only bqto)"
	}
	return fmt.Errorf(
		"ADC account (%s) does not match the active gcloud account (%s)\n"+
			"Fix: log both in as the same account: gcloud auth application-default login, or %s",
		adcEmail, gcloudAccount, fixGcloud,
	)
}

const (
	iapTunnelPermission  = "iap.tunnelInstances.accessViaIAP"
	projectGetPermission = "resourcemanager.projects.get"
)

func CheckIAPTunnel(ctx context.Context, project, email string) (string, error) {
	op := "check the IAP tunnel permission on project " + project
	svc, err := crmv3.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return "", gcperr.Wrap(op, err)
	}
	resp, err := gcperr.Retry(ctx, func() (*crmv3.TestIamPermissionsResponse, error) {
		return svc.Projects.TestIamPermissions("projects/"+project, &crmv3.TestIamPermissionsRequest{
			Permissions: []string{iapTunnelPermission, projectGetPermission},
		}).Context(ctx).Do()
	})
	if err != nil {
		if gcperr.ImpersonationDenied(err) {
			return "", gcperr.CannotImpersonate(email)
		}
		reason, metadata := gcperr.Info(err)
		if disabled := iapCheckError(reason, metadata); disabled != nil {
			return "", disabled
		}
		return "", gcperr.Project(op, project, err)
	}
	if !slices.Contains(resp.Permissions, projectGetPermission) {
		_, err := gcperr.Retry(ctx, func() (*crmv3.Project, error) {
			return svc.Projects.Get("projects/" + project).Context(ctx).Do()
		})
		if err != nil {
			return "", gcperr.Project("read project "+project, project, err)
		}
	}
	return missingIAPWarning(project, email, resp.Permissions), nil
}

func iapCheckError(reason string, metadata map[string]string) error {
	if reason == "SERVICE_DISABLED" {
		project := strings.TrimPrefix(metadata["consumer"], "projects/")
		return fmt.Errorf(
			"cannot check the IAP tunnel permission: bqto needs the %s, which is disabled in project %s\n"+
				"Fix: gcloud services enable %s --project %s",
			metadata["serviceTitle"], project, metadata["service"], project,
		)
	}
	return nil
}

func missingIAPWarning(project, email string, granted []string) string {
	if slices.Contains(granted, iapTunnelPermission) {
		return ""
	}
	member := "user:" + email
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		member = "serviceAccount:" + email
	}
	return fmt.Sprintf(
		"%s lacks %s on project %s, so the tunnel to the bastion may fail\n"+
			"A grant with a condition, such as destination.port == 22, cannot be checked in advance; if that is yours, this is expected\n"+
			"Fix: gcloud projects add-iam-policy-binding %s --member=%s --role=roles/iap.tunnelResourceAccessor",
		email, iapTunnelPermission, project, project, member,
	)
}
