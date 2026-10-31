package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/pilef-co/bqto/ext"
	"google.golang.org/api/option"
	"google.golang.org/api/option/internaloption"
	htransport "google.golang.org/api/transport/http"
)

const (
	EnvVar             = "GOOGLE_CLOUD_QUOTA_PROJECT"
	Header             = "X-Goog-User-Project"
	userProjectDenied  = "USER_PROJECT_DENIED"
	consumerInvalid    = "CONSUMER_INVALID"
	serviceDisabled    = "SERVICE_DISABLED"
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	maxErrorBody       = 1 << 20
)

type projectKey struct{}

var (
	refusedCalls    sync.Map
	listingRefusals sync.Map
)

func WithProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectKey{}, project)
}

func Project(ctx context.Context) string {
	p, _ := ctx.Value(projectKey{}).(string)
	return p
}

const adcHop = "ADC"

func Options(ctx context.Context, opts ...option.ClientOption) []option.ClientOption {
	project := Project(ctx)
	if _, denied := refusedCalls.Load(project); denied || project == "" || !userOwnsQuota() {
		return opts
	}
	adcOpts := authOptions(opts)
	adc, err := adcHopOf(ctx, adcOpts)
	if err != nil {
		return opts
	}
	target, err := targetHop(ctx, adcOpts, project)
	if err != nil {
		return opts
	}
	return withHops(opts, &refusedCalls, target, adc)
}

func ListingOptions(ctx context.Context, opts ...option.ClientOption) []option.ClientOption {
	if !userOwnsQuota() {
		return opts
	}
	adcOpts := authOptions(opts)
	adc, err := adcHopOf(ctx, adcOpts)
	if err != nil {
		return opts
	}
	bare, err := htransport.NewTransport(ctx, withoutQuotaProject{base: http.DefaultTransport}, adcOpts...)
	if err != nil {
		return opts
	}
	hops := []hop{adc, {name: "", rt: bare}}
	if project := Project(ctx); project != "" {
		target, err := targetHop(ctx, adcOpts, project)
		if err != nil {
			return opts
		}
		hops = append([]hop{target}, hops...)
	}
	return withHops(opts, &listingRefusals, hops...)
}

func userOwnsQuota() bool {
	return os.Getenv(EnvVar) == "" && UserCredentials()
}

func authOptions(opts []option.ClientOption) []option.ClientOption {
	return append(slices.Clip(opts), option.WithScopes(cloudPlatformScope), internaloption.EnableNewAuthLibrary())
}

func adcHopOf(ctx context.Context, adcOpts []option.ClientOption) (hop, error) {
	c, _, err := htransport.NewClient(ctx, adcOpts...)
	if err != nil {
		return hop{}, err
	}
	return hop{name: adcHop, rt: c.Transport}, nil
}

func targetHop(ctx context.Context, adcOpts []option.ClientOption, project string) (hop, error) {
	c, _, err := htransport.NewClient(ctx, append(slices.Clip(adcOpts), option.WithQuotaProject(project))...)
	if err != nil {
		return hop{}, err
	}
	return hop{name: project, rt: c.Transport}, nil
}

func withHops(opts []option.ClientOption, refusals *sync.Map, hops ...hop) []option.ClientOption {
	return append(slices.Clip(opts), option.WithHTTPClient(&http.Client{Transport: &fallback{hops: hops, refusals: refusals}}))
}

type withoutQuotaProject struct {
	base http.RoundTripper
}

func (t withoutQuotaProject) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Del(Header)
	return t.base.RoundTrip(clone)
}

type hop struct {
	name string
	rt   http.RoundTripper
}

func (h hop) String() string {
	switch h.name {
	case adcHop:
		return "the quota project of your ADC login"
	case "":
		return "no quota project, as gcloud does"
	}
	return "project " + h.name
}

type fallback struct {
	hops     []hop
	refusals *sync.Map
}

func (f *fallback) RoundTrip(req *http.Request) (*http.Response, error) {
	hops := f.untried(req)
	if len(hops) == 1 {
		return hops[0].rt.RoundTrip(req)
	}
	_, replayable := replay(req)
	resp, err := hops[0].rt.RoundTrip(req)
	if err != nil || !replayable || resp.StatusCode < http.StatusBadRequest {
		return resp, err
	}
	reasons := []string{readReason(resp)}
	if !refused(reasons[0]) {
		return resp, nil
	}
	for i := 1; i < len(hops); i++ {
		ext.Debugf("quota: %s refused the call to %s for API checks and quota (%s); retrying with %s", hops[i-1], req.URL.Host, reasons[i-1], hops[i])
		retry, ok := replay(req)
		if !ok {
			return resp, nil
		}
		next, err := hops[i].rt.RoundTrip(retry)
		if err != nil {
			return resp, nil //nolint:nilerr // the first refusal explains more than a failed retry
		}
		if next.StatusCode >= http.StatusBadRequest {
			if reason := readReason(next); refused(reason) {
				_ = next.Body.Close()
				reasons = append(reasons, reason)
				continue
			}
		}
		_ = resp.Body.Close()
		for j, h := range hops[:i] {
			f.refusals.Store(refusalKey(h.name, req, reasons[j]), true)
		}
		return next, nil
	}
	return resp, nil
}

func (f *fallback) untried(req *http.Request) []hop {
	for i, h := range f.hops[:len(f.hops)-1] {
		_, whole := f.refusals.Load(h.name)
		_, api := f.refusals.Load(refusalKey(h.name, req, serviceDisabled))
		if !whole && !api {
			return f.hops[i:]
		}
	}
	return f.hops[len(f.hops)-1:]
}

func refusalKey(project string, req *http.Request, reason string) string {
	if reason == serviceDisabled {
		return project + " " + req.URL.Host
	}
	return project
}

func refused(reason string) bool {
	switch reason {
	case userProjectDenied, consumerInvalid, serviceDisabled, "BILLING_DISABLED":
		return true
	}
	return false
}

func replay(req *http.Request) (*http.Request, bool) {
	clone := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return clone, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	clone.Body = body
	return clone, true
}

func readReason(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	var e struct {
		Error struct {
			Details []struct {
				Type   string `json:"@type"`
				Reason string `json:"reason"`
			} `json:"details"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	for _, d := range e.Error.Details {
		if strings.HasSuffix(d.Type, "google.rpc.ErrorInfo") && d.Reason != "" {
			return d.Reason
		}
	}
	for _, d := range e.Error.Errors {
		if d.Reason == userProjectDenied {
			return d.Reason
		}
	}
	return ""
}

func UserCredentials() bool {
	b, err := os.ReadFile(adcFile())
	if err != nil {
		return false
	}
	var c struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &c) != nil {
		return false
	}
	return c.Type == "authorized_user" || c.Type == "external_account_authorized_user"
}

func adcFile() string {
	if f := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); f != "" {
		return f
	}
	const name = "application_default_credentials.json"
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "gcloud", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud", name)
}
