package gcperr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/api/googleapi"

	"github.com/pilef-co/bqto/ext"
)

func noWait(t *testing.T) {
	t.Helper()
	delays := RetryDelays
	RetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { RetryDelays = delays })
}

func TestTransient(t *testing.T) {
	tokenErr := &url.Error{Op: "Post", URL: "https://example.com", Err: errors.New(`credentials: status code 503: {"error":{"code":503}}`)}
	tests := []struct {
		desc string
		err  error
		want bool
	}{
		{"503", &googleapi.Error{Code: http.StatusServiceUnavailable}, true},
		{"502", &googleapi.Error{Code: http.StatusBadGateway}, true},
		{"rate limit", &googleapi.Error{Code: http.StatusTooManyRequests, Message: "rate limit exceeded"}, true},
		{"quota", &googleapi.Error{Code: http.StatusTooManyRequests, Message: "Quota exceeded for quota metric"}, false},
		{"not found", &googleapi.Error{Code: http.StatusNotFound}, false},
		{"forbidden", &googleapi.Error{Code: http.StatusForbidden}, false},
		{"token endpoint 503", tokenErr, true},
		{"user login token 503", fmt.Errorf("get x: %w", &auth.Error{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}}), true},
		{"user login token 400", &auth.Error{Response: &http.Response{StatusCode: http.StatusBadRequest}}, false},
		{"status code text elsewhere", errors.New("instance status code 503 is a name"), false},
		{"token endpoint 400", &url.Error{Op: "Post", URL: "https://example.com", Err: errors.New("credentials: status code 400: bad")}, false},
		{"wrapped 503", fmt.Errorf("get x: %w", &googleapi.Error{Code: http.StatusServiceUnavailable}), true},
		{"plain", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("%s: Transient(%v) = %v, want %v", tc.desc, tc.err, got, tc.want)
		}
	}
}

func TestRetry(t *testing.T) {
	unavailable := &googleapi.Error{Code: http.StatusServiceUnavailable}

	t.Run("a transient error is retried until it succeeds", func(t *testing.T) {
		noWait(t)
		calls := 0
		got, err := Retry(context.Background(), func() (string, error) {
			calls++
			if calls < 3 {
				return "", unavailable
			}
			return "ok", nil
		})
		if got != "ok" || err != nil || calls != 3 {
			t.Errorf("Retry = %q, %v after %d calls; want ok after 3", got, err, calls)
		}
	})

	t.Run("a lasting outage stops after the last delay", func(t *testing.T) {
		noWait(t)
		calls := 0
		_, err := Retry(context.Background(), func() (string, error) {
			calls++
			return "", unavailable
		})
		if !errors.Is(err, unavailable) || calls != len(RetryDelays)+1 {
			t.Errorf("Retry = %v after %d calls; want the 503 after %d", err, calls, len(RetryDelays)+1)
		}
	})

	t.Run("a permanent error is not retried", func(t *testing.T) {
		noWait(t)
		calls := 0
		_, _ = Retry(context.Background(), func() (string, error) {
			calls++
			return "", &googleapi.Error{Code: http.StatusForbidden}
		})
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		_, err := Retry(ctx, func() (string, error) {
			calls++
			return "", unavailable
		})
		if err == nil || calls != 1 {
			t.Errorf("Retry = %v after %d calls; want the error after 1", err, calls)
		}
	})
}

func TestATransientTokenErrorAsksToRunAgain(t *testing.T) {
	err := Wrap("check x", &url.Error{Op: "Post", URL: "https://example.com", Err: errors.New("credentials: status code 503: unavailable")})
	if _, fix := SplitFix(err); fix != serverFix {
		t.Errorf("fix = %q, want %q", fix, serverFix)
	}
}

func TestRetryNamesTheCallInTheDebugOutput(t *testing.T) {
	noWait(t)
	var buf strings.Builder
	ext.SetDebug(&buf)
	t.Cleanup(func() { ext.SetDebug(nil) })
	calls := 0
	_, _ = Retry(context.Background(), func() (string, error) {
		calls++
		if calls == 1 {
			return "", &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "busy"}
		}
		return "ok", nil
	})
	out := buf.String()
	for _, want := range []string{"google api: gcperr.TestRetryNamesTheCallInTheDebugOutput took", "busy", "retries in 0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug output %q misses %q", out, want)
		}
	}
}
