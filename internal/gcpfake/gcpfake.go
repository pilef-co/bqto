package gcpfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

type Response struct {
	Status int
	Body   any
}

type Request struct {
	Method      string
	Path        string
	Query       string
	Body        string
	UserProject string
}

type Server struct {
	failf  func(format string, args ...any)
	mu     sync.Mutex
	routes map[string][]Response
	seen   []Request
}

var (
	inUseMu sync.Mutex
	inUse   = map[*[]option.ClientOption]bool{}
)

func Start(t *testing.T, target *[]option.ClientOption) *Server {
	t.Helper()
	inUseMu.Lock()
	if inUse[target] {
		inUseMu.Unlock()
		t.Fatal("gcpfake: another test already points this package's clients at a fake; tests that use gcpfake cannot run with t.Parallel")
	}
	inUse[target] = true
	inUseMu.Unlock()

	s := newServer(t.Errorf)
	srv := httptest.NewServer(s)
	orig := *target
	*target = []option.ClientOption{option.WithEndpoint(srv.URL + "/"), option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "gcpfake"}))}
	t.Cleanup(func() {
		srv.Close()
		*target = orig
		inUseMu.Lock()
		delete(inUse, target)
		inUseMu.Unlock()
		s.reportUnused()
	})
	return s
}

func newServer(failf func(format string, args ...any)) *Server {
	return &Server{failf: failf, routes: map[string][]Response{}}
}

func (s *Server) On(method, path string, responses ...Response) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + path
	s.routes[key] = append(s.routes[key], responses...)
	return s
}

func (s *Server) OK(method, path string, body any) *Server {
	return s.On(method, path, Response{Status: http.StatusOK, Body: body})
}

func (s *Server) Err(method, path string, status int, reason, message string) *Server {
	return s.On(method, path, Error(status, reason, message))
}

func Error(status int, reason, message string) Response {
	return Response{Status: status, Body: map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"errors":  []map[string]any{{"reason": reason, "message": message}},
		},
	}}
}

func ErrorInfo(status int, reason, message string, metadata map[string]string) Response {
	return Response{Status: status, Body: map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"details": []map[string]any{{
				"@type":    "type.googleapis.com/google.rpc.ErrorInfo",
				"reason":   reason,
				"domain":   "googleapis.com",
				"metadata": metadata,
			}},
		},
	}}
}

func UserADC(t *testing.T) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "application_default_credentials.json")
	if err := os.WriteFile(f, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", f)
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "")
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	if strings.HasPrefix(path, "//") {
		path = path[1:]
	}
	req := Request{Method: r.Method, Path: path, Query: r.URL.RawQuery, Body: string(body), UserProject: r.Header.Get("X-Goog-User-Project")}

	resp, ok := s.next(req)
	switch {
	case !ok:
		s.failf("gcpfake: unexpected request %s %s?%s", req.Method, req.Path, req.Query)
		resp = Error(http.StatusNotImplemented, "notImplemented", "no fake for "+req.Method+" "+req.Path)
	case resp.Status == 0:
		s.failf("gcpfake: the response to %s %s has no status", req.Method, req.Path)
		resp = Error(http.StatusInternalServerError, "internal", "gcpfake response without a status")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_ = json.NewEncoder(w).Encode(resp.Body)
}

func (s *Server) next(req Request) (Response, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	got, _ := url.ParseQuery(req.Query)
	var keys []string
	for key, queue := range s.routes {
		if len(queue) > 0 && routeMatches(key, req.Method+" "+req.Path, got) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return Response{}, false
	}
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	queue := s.routes[keys[0]]
	s.routes[keys[0]] = queue[1:]
	return queue[0], true
}

func routeMatches(key, methodPath string, got url.Values) bool {
	route, query, _ := strings.Cut(key, "?")
	if route != methodPath {
		return false
	}
	want, err := url.ParseQuery(query)
	if err != nil {
		return false
	}
	for name, values := range want {
		if !slices.Equal(got[name], values) {
			return false
		}
	}
	return true
}

func (s *Server) reportUnused() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var unused []string
	for key, queue := range s.routes {
		if len(queue) > 0 {
			unused = append(unused, fmt.Sprintf("%s (%d left)", key, len(queue)))
		}
	}
	slices.Sort(unused)
	if len(unused) > 0 {
		s.failf("gcpfake: responses never requested: %s", strings.Join(unused, ", "))
	}
}
