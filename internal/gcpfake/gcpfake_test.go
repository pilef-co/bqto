package gcpfake

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failures []string

func (f *failures) failf(format string, args ...any) {
	*f = append(*f, fmt.Sprintf(format, args...))
}

func serve(t *testing.T, s *Server, method, target string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	body, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(body)
}

func TestServer(t *testing.T) {
	t.Run("a queue is served in order and then runs out", func(t *testing.T) {
		var f failures
		s := newServer(f.failf).OK("GET", "/v1/x", "first").OK("GET", "/v1/x", "second")
		for _, want := range []string{"first", "second"} {
			if code, body := serve(t, s, "GET", "/v1/x"); code != http.StatusOK || !strings.Contains(body, want) {
				t.Errorf("got %d %s, want %s", code, body, want)
			}
		}
		if code, _ := serve(t, s, "GET", "/v1/x"); code != http.StatusNotImplemented || len(f) != 1 {
			t.Errorf("a third call got %d and failures %v, want 501 and one failure", code, f)
		}
	})

	t.Run("an unknown route fails", func(t *testing.T) {
		var f failures
		serve(t, newServer(f.failf), "GET", "/v1/nothing")
		if len(f) != 1 || !strings.Contains(f[0], "unexpected request GET /v1/nothing") {
			t.Errorf("failures = %v", f)
		}
	})

	t.Run("responses never requested are reported", func(t *testing.T) {
		var f failures
		newServer(f.failf).OK("GET", "/v1/x", "unused").reportUnused()
		if len(f) != 1 || !strings.Contains(f[0], "GET /v1/x (1 left)") {
			t.Errorf("failures = %v", f)
		}
	})

	t.Run("the query of a route must match", func(t *testing.T) {
		var f failures
		s := newServer(f.failf).OK("GET", "/v1/x?network=a", "a")
		if code, _ := serve(t, s, "GET", "/v1/x?network=b&alt=json"); code != http.StatusNotImplemented {
			t.Errorf("a different network got %d, want 501", code)
		}
		if code, _ := serve(t, s, "GET", "/v1/x?network=a&alt=json"); code != http.StatusOK {
			t.Errorf("the right network got %d, want 200", code)
		}
		if got := s.Requests()[1].Query; got != "network=a&alt=json" {
			t.Errorf("recorded query = %q", got)
		}
	})

	t.Run("a doubled slash inside the path is not cleaned", func(t *testing.T) {
		var f failures
		s := newServer(f.failf).OK("GET", "/v1/locations/instances", "x")
		if code, _ := serve(t, s, "GET", "/v1/locations//instances"); code != http.StatusNotImplemented {
			t.Errorf("got %d, want 501", code)
		}
		if code, _ := serve(t, s, "GET", "//v1/locations/instances"); code != http.StatusOK {
			t.Errorf("the leading slash of the endpoint got %d, want 200", code)
		}
	})

	t.Run("a response without a status fails instead of panicking", func(t *testing.T) {
		var f failures
		s := newServer(f.failf).On("GET", "/v1/x", Response{Body: "x"})
		if code, _ := serve(t, s, "GET", "/v1/x"); code != http.StatusInternalServerError || len(f) != 1 {
			t.Errorf("got %d and failures %v", code, f)
		}
	})
}
