package gcperr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func NotFound(err error) bool {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return gerr.Code == 404
	}
	return status.Code(err) == codes.NotFound
}

func Get(kind, name, project, listCmd string, err error) error {
	if InvalidProject(err) {
		return MissingProject(project)
	}
	if NotFound(err) {
		return Missing(kind, name, project, listCmd)
	}
	return Wrap(fmt.Sprintf("get %s %s in project %s", kind, name, project), err)
}

type Error struct {
	Op  string
	Err error
	Fix string
}

func (e *Error) Error() string {
	msg := e.Op + ": " + Message(e.Err)
	if e.Fix != "" {
		msg += "\nFix: " + e.Fix
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func Wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*Error](err); ok {
		return err
	}
	if strings.Contains(err.Error(), "\nFix: ") {
		return fmt.Errorf("%s: %w", op, err)
	}
	return &Error{Op: op, Err: err, Fix: fixFor(err)}
}

func WrapForbidden(op string, err error, fix string) error {
	wrapped := Wrap(op, err)
	var e *Error
	if errors.As(wrapped, &e) && e.Fix == genericForbidden {
		e.Fix = fix
	}
	return wrapped
}

var permissionName = regexp.MustCompile(`'([a-z0-9]+(?:\.[a-zA-Z0-9]+){2,})'`)

const genericForbidden = "check that your account has the roles bqto needs, and that the project and resource names are right; " + permissionsDoc

const quotaFix = "a quota of the project is used up; free some, or ask for more in the console under IAM & Admin, Quotas"

const rateFix = "Google is limiting how fast the calls come; wait a minute and run bqto again"

const serverFix = "Google returned a server error; run bqto again in a minute"

const networkFix = "Google could not be reached; check your network or proxy and run bqto again"

const invalidProjectFix = "check the project ID with gcloud projects list: the project does not exist, or your account cannot see it"

const permissionsDoc = "the permissions page of the bqto docs (docs/permissions.md) lists the roles bqto needs"

func fixFor(err error) string {
	if ImpersonationDenied(err) {
		return impersonationFix(unnamedServiceAccount)
	}
	if needsReauth(err) {
		return "Google asks you to sign in again: gcloud auth login, then gcloud auth application-default login"
	}
	reason, metadata := Info(err)
	project := strings.TrimPrefix(metadata["consumer"], "projects/")
	msg := Message(err)
	lower := strings.ToLower(msg)
	switch {
	case reason == "SERVICE_DISABLED":
		return fmt.Sprintf("gcloud services enable %s --project %s", metadata["service"], project)
	case reason == "BILLING_DISABLED" || strings.Contains(lower, "billing account") || strings.Contains(lower, "billing must be enabled") ||
		strings.Contains(lower, "requires billing") || strings.Contains(lower, "billing is disabled") || strings.Contains(lower, "billing to be enabled"):
		return "enable billing on the project, or ask its owner to"
	case reason == "SECURITY_POLICY_VIOLATED" || strings.Contains(msg, "VPC Service Controls") || strings.Contains(msg, "vpcServiceControls"):
		return "a VPC Service Controls perimeter blocked the call; ask the perimeter's admin for an ingress rule for your identity"
	case reason == "USER_PROJECT_DENIED":
		return quotaProjectDeniedFix(project)
	case InvalidProject(err):
		return invalidProjectFix
	}
	switch limit(err, reason, lower) {
	case "rate":
		return rateFix
	case "quota":
		return quotaFix
	}
	switch code(err) {
	case http.StatusForbidden:
		if m := permissionName.FindStringSubmatch(msg); m != nil {
			if strings.Contains(lower, "may not exist") {
				return fmt.Sprintf("check the project and resource name, which may not exist; if they are right, ask for a role that grants %s; %s", m[1], permissionsDoc)
			}
			return fmt.Sprintf("ask for a role that grants %s; %s", m[1], permissionsDoc)
		}
		return genericForbidden
	case http.StatusUnauthorized:
		return "gcloud auth application-default login"
	case http.StatusNotFound:
		return "check the name and the project: it may be mistyped or deleted"
	}
	if code(err) >= http.StatusInternalServerError || Transient(err) {
		return serverFix
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the call timed out; check your network and run bqto again"
	}
	if expiredLogin(err) {
		return "your login expired: gcloud auth application-default login"
	}
	if !errors.Is(err, context.Canceled) && unreachable(err) {
		return networkFix
	}
	return ""
}

func quotaProjectDeniedFix(project string) string {
	if project == "" {
		project = "the quota project"
	} else {
		project = "project " + project
	}
	return fmt.Sprintf("your account cannot use %s for API checks and quota: ask for roles/serviceusage.serviceUsageConsumer on it, or export GOOGLE_CLOUD_QUOTA_PROJECT=PROJECT, a project where you have that role and the API is enabled", project)
}

func needsReauth(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid_rapt") || strings.Contains(msg, "reauth related error")
}

func expiredLogin(err error) bool {
	var retrieve *oauth2.RetrieveError
	msg := err.Error()
	return errors.As(err, &retrieve) || strings.Contains(msg, "oauth2: cannot fetch token") || strings.Contains(msg, `auth: "invalid_grant"`)
}

func unreachable(err error) bool {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var urlErr *url.Error
	return errors.As(err, &opErr) || errors.As(err, &dnsErr) || errors.As(err, &urlErr)
}

func limit(err error, reason, lower string) string {
	reasons := []string{reason}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		for _, item := range gerr.Errors {
			reasons = append(reasons, item.Reason)
		}
	}
	for _, r := range reasons {
		switch strings.ToUpper(r) {
		case "RATELIMITEXCEEDED", "USERRATELIMITEXCEEDED", "RATE_LIMIT_EXCEEDED":
			return "rate"
		case "QUOTAEXCEEDED", "DAILYLIMITEXCEEDED", "QUOTA_EXCEEDED":
			return "quota"
		}
	}
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "per minute"):
		return "rate"
	case strings.Contains(lower, "quota") && (strings.Contains(lower, "exceeded") || strings.Contains(lower, "exhausted")):
		return "quota"
	case gerr != nil && gerr.Code == http.StatusTooManyRequests:
		return "rate"
	}
	return ""
}

func code(err error) int {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return gerr.Code
	}
	switch status.Code(err) {
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.NotFound:
		return http.StatusNotFound
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	}
	return 0
}

func SplitFix(err error) (message, fix string) {
	message, fix, _ = strings.Cut(err.Error(), "\nFix: ")
	return message, fix
}

func Info(err error) (string, map[string]string) {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		for _, d := range gerr.Details {
			m, ok := d.(map[string]any)
			if !ok || !strings.HasSuffix(fmt.Sprint(m["@type"]), "google.rpc.ErrorInfo") {
				continue
			}
			metadata := map[string]string{}
			if md, ok := m["metadata"].(map[string]any); ok {
				for k, v := range md {
					metadata[k] = fmt.Sprint(v)
				}
			}
			return fmt.Sprint(m["reason"]), metadata
		}
		return "", nil
	}
	if st, ok := status.FromError(err); ok {
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				return info.GetReason(), info.GetMetadata()
			}
		}
	}
	return "", nil
}

func InvalidProject(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(Message(err)), "project specified in the request is invalid")
}

func MissingProject(project string) error {
	return fmt.Errorf("project %s does not exist, or your account cannot see it\nFix: check the ID with gcloud projects list; if the project exists but is not listed, ask for resourcemanager.projects.get on it (roles/browser)", project)
}

func Project(op, project string, err error) error {
	if err == nil {
		return nil
	}
	if NotFound(err) || InvalidProject(err) || deniedOnProject(err) {
		return MissingProject(project)
	}
	return Wrap(op, err)
}

func deniedOnProject(err error) bool {
	if code(err) != http.StatusForbidden {
		return false
	}
	reason, _ := Info(err)
	return (reason == "" || reason == "IAM_PERMISSION_DENIED") && strings.HasSuffix(fixFor(err), permissionsDoc)
}

func ImpersonationDenied(err error) bool {
	if _, ok := errors.AsType[*googleapi.Error](err); ok || err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "iam.serviceAccounts.getAccessToken") {
		return true
	}
	m := tokenStatus.FindStringSubmatch(msg)
	return m != nil && m[1] == "403" && strings.Contains(msg, "impersonate: status code")
}

func CannotImpersonate(serviceAccount string) error {
	return errors.New(cannotImpersonate(serviceAccount) + "\nFix: " + impersonationFix(serviceAccount))
}

func cannotImpersonate(serviceAccount string) string {
	return "your account cannot impersonate " + serviceAccount
}

func impersonationFix(serviceAccount string) string {
	return "ask for roles/iam.serviceAccountTokenCreator on " + serviceAccount + " and wait a few minutes for the grant to apply, or stop impersonating: gcloud config unset auth/impersonate_service_account"
}

const unnamedServiceAccount = "the service account it is set to impersonate"

func Missing(kind, name, project, listCmd string) error {
	return fmt.Errorf("%s %s not found in project %s\nFix: %s", kind, name, project, listCmd)
}

func Message(err error) string {
	if ImpersonationDenied(err) {
		return cannotImpersonate(unnamedServiceAccount)
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Message != "" {
		return clean(gerr.Message)
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Message() != "" {
		return clean(st.Message())
	}
	return clean(err.Error())
}

var (
	apiPrefix = regexp.MustCompile(`googleapi: Error \d+: `)
	reason    = regexp.MustCompile(`[.,]?, [a-zA-Z][a-zA-Z0-9_]*$`)
)

func clean(msg string) string {
	msg = apiPrefix.ReplaceAllString(msg, "")
	msg = strings.TrimSpace(reason.ReplaceAllString(msg, "."))
	return strings.TrimSuffix(msg, ".")
}
