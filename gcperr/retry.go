package gcperr

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth"

	"github.com/pilef-co/bqto/ext"
)

var RetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

func Retry[T any](ctx context.Context, call func() (T, error)) (T, error) {
	caller := callerName(2)
	for attempt := 0; ; attempt++ {
		start := time.Now()
		v, err := call()
		ext.Debugf("google api: %s took %s%s", caller, time.Since(start).Round(time.Millisecond), outcome(err))
		if err == nil || attempt >= len(RetryDelays) || !Transient(err) {
			return v, err
		}
		ext.Debugf("google api: %s retries in %s", caller, RetryDelays[attempt])
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(RetryDelays[attempt]):
		}
	}
}

var tokenStatus = regexp.MustCompile(`(?:credentials|impersonate): status code (\d{3})`)

func Transient(err error) bool {
	if err == nil {
		return false
	}
	if c := code(err); c != 0 {
		if c == http.StatusTooManyRequests {
			reason, _ := Info(err)
			return limit(err, reason, strings.ToLower(Message(err))) != "quota"
		}
		return transientStatus(c)
	}
	if ae, ok := errors.AsType[*auth.Error](err); ok && ae.Response != nil {
		return transientStatus(ae.Response.StatusCode)
	}
	if m := tokenStatus.FindStringSubmatch(err.Error()); m != nil {
		c, _ := strconv.Atoi(m[1])
		return transientStatus(c)
	}
	return false
}

func transientStatus(c int) bool {
	switch c {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func RetryDo[T, O any](ctx context.Context, do func(...O) (T, error)) (T, error) {
	return Retry(ctx, func() (T, error) { return do() })
}

func callerName(skip int) string {
	for i := skip; i < skip+4; i++ {
		pc, _, _, ok := runtime.Caller(i)
		if !ok {
			break
		}
		name := runtime.FuncForPC(pc).Name()
		if strings.Contains(name, "gcperr.Retry") {
			continue
		}
		name = name[strings.LastIndex(name, "/")+1:]
		if j := strings.Index(name, ".func"); j > 0 {
			name = name[:j]
		}
		return name
	}
	return "call"
}

func outcome(err error) string {
	if err == nil {
		return ""
	}
	return ": " + Message(err)
}
