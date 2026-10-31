package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/ext"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type fakeGitHub struct {
	tag, body string
	status    int
	archive   []byte
	checksums string
	requests  []string
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("the request to %s carries credentials", r.URL.Path)
		}
		switch {
		case r.URL.Path == "/repos/pilef-co/bqto/releases/latest":
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			fmt.Fprintf(w, `{"tag_name":%q,"body":%q}`, f.tag, f.body)
		case r.URL.Path == "/pilef-co/bqto/releases/download/"+f.tag+"/"+ArchiveName("bqto", ext.ShortVersion(f.tag)):
			_, _ = w.Write(f.archive)
		case r.URL.Path == "/pilef-co/bqto/releases/download/"+f.tag+"/checksums.txt":
			fmt.Fprint(w, f.checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func githubFor(srv *httptest.Server, have string) GitHub {
	return GitHub{API: srv.URL, Download: srv.URL, Client: srv.Client(), Have: have}
}

func TestLatest(t *testing.T) {
	tests := []struct {
		name, tag, body, have string
		want                  ext.Release
	}{
		{"newer", "v0.12.0", "", "v0.11.0", ext.Release{Version: "0.12.0", Newer: true, Ref: "v0.12.0"}},
		{"same", "v0.11.0", "", "v0.11.0", ext.Release{Version: "0.11.0", Ref: "v0.11.0"}},
		{"older than a go install pseudo-version", "v0.11.0", "", "v0.11.1-0.20261027120000-abcdef123456", ext.Release{Version: "0.11.0", Ref: "v0.11.0"}},
		{"pseudo-version before the release", "v0.11.1", "", "v0.11.1-0.20261027120000-abcdef123456", ext.Release{Version: "0.11.1", Newer: true, Ref: "v0.11.1"}},
		{"security yes", "v0.12.0", "## Fixes\n\nSecurity: yes\n", "v0.11.0", ext.Release{Version: "0.12.0", Newer: true, Security: true, Ref: "v0.12.0"}},
		{"security version, affected", "v0.13.0", "security: 0.12.0", "v0.11.0", ext.Release{Version: "0.13.0", Newer: true, Security: true, Ref: "v0.13.0"}},
		{"security version, already fixed", "v0.13.0", "Security: 0.12.0", "v0.12.0", ext.Release{Version: "0.13.0", Newer: true, Ref: "v0.13.0"}},
		{"security in prose is not the mark", "v0.12.0", "Security fixes: none", "v0.11.0", ext.Release{Version: "0.12.0", Newer: true, Ref: "v0.12.0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitHub{tag: tc.tag, body: tc.body}
			got, err := githubFor(f.server(t), tc.have).Latest(context.Background())
			if err != nil || got != tc.want {
				t.Errorf("Latest() = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestLatestRefusesAPrivateRepoOrABadTag(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		f := &fakeGitHub{status: status}
		_, err := githubFor(f.server(t), "v0.11.0").Latest(context.Background())
		if !errors.Is(err, errNoPublicRelease) {
			t.Errorf("status %d: err = %v, want errNoPublicRelease", status, err)
		}
	}
	for _, tag := range []string{"v0.12.0/../../x", "0.12.0", "v0.12", "v0.12.0-rc1", "v01.2.3"} {
		f := &fakeGitHub{tag: tag}
		if _, err := githubFor(f.server(t), "v0.11.0").Latest(context.Background()); err == nil {
			t.Errorf("tag %q: no error", tag)
		}
	}
}

func TestCheckTimesOut(t *testing.T) {
	u := ext.Updater{
		Current: func() string { return "v0.11.0" },
		Timeout: 20 * time.Millisecond,
		Latest: func(ctx context.Context) (ext.Release, error) {
			<-ctx.Done()
			return ext.Release{}, ctx.Err()
		},
	}
	start := time.Now()
	st, err := Check(context.Background(), u)
	if !errors.Is(err, context.DeadlineExceeded) || st.Newer || time.Since(start) > time.Second {
		t.Errorf("Check() = %+v, %v after %v", st, err, time.Since(start))
	}
}

func TestACheckoutBuildIsADevBuild(t *testing.T) {
	for _, have := range []string{"v0.11.1-0.20261027120000-abcdef123456", "v0.11.1-0.20261027120000-abcdef123456+dirty", "v0.11.0+dirty", "v0.12.0-rc1"} {
		called := false
		u := ext.Updater{Name: "bqto", Current: func() string { return have }, Latest: func(context.Context) (ext.Release, error) {
			called = true
			return ext.Release{Version: "0.11.1", Newer: true}, nil
		}}
		st, err := Check(context.Background(), u)
		if err == nil || called || st.Version != strings.TrimPrefix(have, "v") {
			t.Errorf("Check(%s) = %+v, %v, Latest called %v", have, st, err, called)
		}
		_, exe := installed(t)
		err = Update(context.Background(), Options{Updater: u, Exe: exe, Out: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "development build") || called {
			t.Errorf("Update(%s) = %v, Latest called %v", have, err, called)
		}
	}
}

func TestOnlyHTTPSRedirectsAreFollowed(t *testing.T) {
	check := DefaultGitHub("v0.11.0").Client.CheckRedirect
	for url, ok := range map[string]bool{"https://objects.githubusercontent.com/x": true, "http://objects.githubusercontent.com/x": false} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if err := check(req, nil); (err == nil) != ok {
			t.Errorf("redirect to %s: %v", url, err)
		}
	}
}

func TestARateLimitSaysToWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, err := githubFor(srv, "v0.11.0").Latest(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "\nFix: wait an hour and run the command again") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckSkipsADevBuild(t *testing.T) {
	called := false
	u := ext.Updater{Current: func() string { return "dev" }, Latest: func(context.Context) (ext.Release, error) {
		called = true
		return ext.Release{}, nil
	}}
	if _, err := Check(context.Background(), u); err == nil || called {
		t.Errorf("Check(dev) = %v, Latest called %v", err, called)
	}
}

func TestNotice(t *testing.T) {
	tests := []struct {
		st   Status
		want string
	}{
		{Status{Version: "0.11.0", Latest: "0.11.0"}, ""},
		{Status{Version: "0.11.0", Latest: "0.12.0", Newer: true}, "bqto 0.12.0 is available (you have 0.11.0).\nFix: bqto update\n"},
		{Status{Version: "0.11.1-0.20261027120000-abcdef123456", Latest: "0.11.1", Newer: true}, "bqto 0.11.1 is available (you have 0.11.1-0.20261027120000-abcdef123456).\nFix: bqto update\n"},
		{Status{Version: "0.11.0", Latest: "0.12.0", Newer: true, Security: true}, "bqto 0.12.0 fixes a security issue in your version.\nFix: bqto update\n"},
	}
	for _, tc := range tests {
		var out strings.Builder
		Notice(&out, "bqto", tc.st)
		if out.String() != tc.want {
			t.Errorf("Notice(%+v) = %q, want %q", tc.st, out.String(), tc.want)
		}
	}
}

func installed(t *testing.T) (dir, exe string) {
	t.Helper()
	dir = t.TempDir()
	exe = filepath.Join(dir, "bqto")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, exe
}

func releaseFixture(t *testing.T, binary string) *fakeGitHub {
	t.Helper()
	archive := tarGz(t, map[string]string{"README.md": "readme", "bqto": binary})
	name := ArchiveName("bqto", "0.12.0")
	return &fakeGitHub{tag: "v0.12.0", archive: archive, checksums: sum([]byte("other")) + "  bqto_0.12.0_other.tar.gz\n" + sum(archive) + "  " + name + "\n"}
}

func updaterFor(g GitHub) ext.Updater {
	return ext.Updater{Name: "bqto", Current: func() string { return g.Have }, Latest: g.Latest, Download: g.Fetch, DirFix: "move bqto"}
}

func TestUpdateReplacesThePlainBinary(t *testing.T) {
	f := releaseFixture(t, "new")
	g := githubFor(f.server(t), "v0.11.0")
	dir, exe := installed(t)
	link := filepath.Join(t.TempDir(), "bqto")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Update(context.Background(), Options{Updater: updaterFor(g), Exe: resolved, Out: &out}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(link)
	if err != nil || string(b) != "new" {
		t.Errorf("after the update the link reads %q, %v", b, err)
	}
	fi, err := os.Stat(exe)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced: %v, %v", fi.Mode(), err)
	}
	if !strings.Contains(out.String(), "Updated bqto from 0.11.0 to 0.12.0: "+resolved) {
		t.Errorf("out = %q", out.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left behind in %s: %v", dir, entries)
	}
}

func TestUpdateKeepsTheMode(t *testing.T) {
	f := releaseFixture(t, "new")
	_, exe := installed(t)
	if err := os.Chmod(exe, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, %v; want 0750", fi.Mode(), err)
	}
}

func TestACancelledUpdateKeepsTheBinary(t *testing.T) {
	f := releaseFixture(t, "new")
	g := githubFor(f.server(t), "v0.11.0")
	ctx, cancel := context.WithCancel(context.Background())
	u := updaterFor(g)
	u.Download = func(c context.Context, r ext.Release, dir string) (string, error) {
		archive, err := g.Fetch(c, r, dir)
		cancel()
		return archive, err
	}
	dir, exe := installed(t)
	if err := Update(ctx, Options{Updater: u, Exe: exe, Out: &strings.Builder{}}); err == nil {
		t.Error("a cancelled update succeeded")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Errorf("binary = %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestUpdateRefusesABadChecksum(t *testing.T) {
	tests := map[string]func(f *fakeGitHub){
		"wrong sum": func(f *fakeGitHub) {
			f.checksums = sum([]byte("x")) + "  " + ArchiveName("bqto", "0.12.0") + "\n"
		},
		"no line for the archive": func(f *fakeGitHub) { f.checksums = sum(f.archive) + "  bqto_0.12.0_other.tar.gz\n" },
		"empty":                   func(f *fakeGitHub) { f.checksums = "" },
		"short sum":               func(f *fakeGitHub) { f.checksums = "abcd  " + ArchiveName("bqto", "0.12.0") + "\n" },
		"sum for a path":          func(f *fakeGitHub) { f.checksums = sum(f.archive) + "  ../" + ArchiveName("bqto", "0.12.0") + "\n" },
	}
	for name, spoil := range tests {
		t.Run(name, func(t *testing.T) {
			f := releaseFixture(t, "evil")
			spoil(f)
			_, exe := installed(t)
			err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}})
			if err == nil || !strings.Contains(err.Error(), "\nFix: ") {
				t.Errorf("err = %v, want an error with a Fix line", err)
			}
			if b, _ := os.ReadFile(exe); string(b) != "old" {
				t.Errorf("the binary was replaced with %q", b)
			}
		})
	}
}

func TestUpdateRefusesAnArchiveWithoutTheBinary(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no bqto":         {"README.md": "x"},
		"bqto in a dir":   {"sub/bqto": "x"},
		"path traversal":  {"../bqto": "x"},
		"absolute path":   {"/usr/local/bin/bqto": "x"},
		"lookalike names": {"bqto.exe": "x", "bqtoo": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			f := releaseFixture(t, "")
			f.archive = tarGz(t, files)
			f.checksums = sum(f.archive) + "  " + ArchiveName("bqto", "0.12.0") + "\n"
			_, exe := installed(t)
			err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}})
			if err == nil || !strings.Contains(err.Error(), "holds no bqto binary") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestUpdateRefusesASymlinkEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "bqto", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.Close()
	_ = gz.Close()
	f := releaseFixture(t, "")
	f.archive = buf.Bytes()
	f.checksums = sum(f.archive) + "  " + ArchiveName("bqto", "0.12.0") + "\n"
	_, exe := installed(t)
	err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}})
	if err == nil {
		t.Error("a symlink entry was accepted")
	}
}

func TestUpdateSaysWhenThereIsNothingNewer(t *testing.T) {
	f := releaseFixture(t, "new")
	_, exe := installed(t)
	var out strings.Builder
	err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.12.0")), Exe: exe, Out: &out})
	if err != nil || out.String() != "bqto 0.12.0 is the newest version, so there is nothing to update.\n" {
		t.Errorf("Update() = %v, %q", err, out.String())
	}
	for _, p := range f.requests {
		if strings.Contains(p, "/download/") {
			t.Errorf("downloaded %s although nothing is newer", p)
		}
	}
}

func TestUpdateOfAPrivateRepoPointsToTheReadme(t *testing.T) {
	f := &fakeGitHub{status: http.StatusNotFound}
	_, exe := installed(t)
	err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}})
	if err == nil || !strings.HasSuffix(err.Error(), "\nFix: download the release by hand, as the README shows under Build and install") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateRefusesADevBuild(t *testing.T) {
	_, exe := installed(t)
	u := ext.Updater{Name: "bqto", Current: func() string { return "dev" }}
	err := Update(context.Background(), Options{Updater: u, Exe: exe, Out: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "development build") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateNeedsAWritableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := releaseFixture(t, "new")
	dir, exe := installed(t)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}})
	if !errors.Is(err, errNoWrite) || !strings.HasSuffix(err.Error(), "\nFix: move bqto") {
		t.Errorf("err = %v", err)
	}
	for _, p := range f.requests {
		if strings.Contains(p, "/download/") {
			t.Errorf("downloaded %s into a read-only folder", p)
		}
	}
	f.tag = "v0.11.0"
	var out strings.Builder
	if err := Update(context.Background(), Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &out}); err != nil || !strings.Contains(out.String(), "nothing to update") {
		t.Errorf("up to date in a read-only folder: %v, %q", err, out.String())
	}
}

func readOnly(t *testing.T) (dir, exe string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir, exe = installed(t)
	if err := os.Chmod(exe, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir, exe
}

func fakeSudo(t *testing.T, dir string, ran *[]string) Runner {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) error {
		*ran = append([]string{name}, args...)
		if name != "sudo" || args[0] != "/bin/sh" {
			return fmt.Errorf("ran %s %s, want sudo /bin/sh", name, args[0])
		}
		if ctx.Done() != nil {
			return errors.New("sudo runs under a context that can cancel it")
		}
		if n := len(args); args[n-2] != strconv.Itoa(os.Getuid()) || args[n-1] != strconv.Itoa(os.Getgid()) {
			return fmt.Errorf("owner %s:%s, want %d:%d", args[n-2], args[n-1], os.Getuid(), os.Getgid())
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
		defer func() { _ = os.Chmod(dir, 0o555) }()
		return exec.CommandContext(ctx, args[0], args[1:]...).Run()
	}
}

func TestUpdateWithSudoReplacesABinaryInAReadOnlyFolder(t *testing.T) {
	f := releaseFixture(t, "new")
	dir, exe := readOnly(t)
	var ran []string
	var out, errs strings.Builder
	o := Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &out, Err: &errs, Sudo: Sudo{Run: fakeSudo(t, dir, &ran), Terminal: true, Found: true}}
	if err := Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(exe); err != nil || string(b) != "new" {
		t.Errorf("binary = %q, %v", b, err)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, %v; want 0750", fi.Mode(), err)
	}
	if errs.String() != "bqto needs sudo to replace "+exe+"\n" {
		t.Errorf("stderr = %q", errs.String())
	}
	if len(ran) != 11 || ran[5] != "0750" || ran[8] != exe || strings.HasPrefix(ran[6], dir) {
		t.Errorf("ran %q", ran)
	}
	if _, err := os.Stat(ran[6]); !os.IsNotExist(err) {
		t.Errorf("the staged binary %s is left behind: %v", ran[6], err)
	}
	if !strings.Contains(out.String(), "Updated bqto from 0.11.0 to 0.12.0: "+exe) {
		t.Errorf("out = %q", out.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind in %s: %v", dir, entries)
	}
}

func TestAFailedSudoKeepsTheBinary(t *testing.T) {
	f := releaseFixture(t, "new")
	dir, exe := readOnly(t)
	failing := func(context.Context, string, ...string) error { return errors.New("exit status 1") }
	var errs strings.Builder
	o := Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}, Err: &errs, Sudo: Sudo{Run: failing, Terminal: true, Found: true}}
	err := Update(context.Background(), o)
	if err == nil || err.Error() != "sudo did not replace "+exe+" (exit status 1), so the old binary stays\nFix: run bqto update again and enter your password, or move bqto" {
		t.Errorf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Errorf("binary = %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind in %s: %v", dir, entries)
	}
}

func TestSudoRunsOnlyAfterTheChecksum(t *testing.T) {
	f := releaseFixture(t, "evil")
	f.checksums = sum([]byte("x")) + "  " + ArchiveName("bqto", "0.12.0") + "\n"
	_, exe := readOnly(t)
	called := false
	run := func(context.Context, string, ...string) error { called = true; return nil }
	var errs strings.Builder
	o := Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}, Err: &errs, Sudo: Sudo{Run: run, Terminal: true, Found: true}}
	if err := Update(context.Background(), o); err == nil || called || errs.Len() > 0 {
		t.Errorf("err = %v, sudo called %v, stderr %q", err, called, errs.String())
	}
}

func TestUpdateRefusesSudoItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		sudo Sudo
		want string
	}{
		{"no terminal", Sudo{Found: true}, "but there is no terminal for sudo to ask for your password\nFix: run bqto update in a terminal, or move bqto"},
		{"sudo missing", Sudo{Terminal: true}, "but sudo is not on your PATH\nFix: move bqto"},
		{"already root", Sudo{Terminal: true, Found: true, Root: true}, "because you cannot write to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := releaseFixture(t, "new")
			_, exe := readOnly(t)
			called := false
			tc.sudo.Run = func(context.Context, string, ...string) error { called = true; return nil }
			o := Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}, Err: &strings.Builder{}, Sudo: tc.sudo}
			err := Update(context.Background(), o)
			if !errors.Is(err, errNoWrite) || !strings.Contains(err.Error(), tc.want) || strings.Count(err.Error(), "\n") != 1 || called {
				t.Errorf("err = %v, sudo called %v", err, called)
			}
			for _, p := range f.requests {
				if strings.Contains(p, "/download/") {
					t.Errorf("downloaded %s although sudo cannot run", p)
				}
			}
			if b, _ := os.ReadFile(exe); string(b) != "old" {
				t.Errorf("binary = %q", b)
			}
		})
	}
}

func TestAWritableFolderNeedsNoSudo(t *testing.T) {
	f := releaseFixture(t, "new")
	_, exe := installed(t)
	called := false
	run := func(context.Context, string, ...string) error { called = true; return nil }
	var errs strings.Builder
	o := Options{Updater: updaterFor(githubFor(f.server(t), "v0.11.0")), Exe: exe, Out: &strings.Builder{}, Err: &errs, Sudo: Sudo{Run: run, Terminal: true, Found: true}}
	if err := Update(context.Background(), o); err != nil || called || errs.Len() > 0 {
		t.Errorf("err = %v, sudo called %v, stderr %q", err, called, errs.String())
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Errorf("binary = %q", b)
	}
}

func TestUpdateRunsTheInstallTool(t *testing.T) {
	for _, tc := range []struct {
		method Method
		want   string
	}{
		{Homebrew, "brew upgrade bqto"},
		{GoInstall, "go install github.com/pilef-co/bqto/cmd/bqto@latest"},
	} {
		var ran []string
		var out strings.Builder
		run := func(_ context.Context, name string, args ...string) error {
			ran = append([]string{name}, args...)
			return nil
		}
		if err := Update(context.Background(), Options{Method: tc.method, Out: &out, Run: run}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(ran, " ") != tc.want || !strings.HasSuffix(out.String(), "so bqto update runs: "+tc.want+"\n") {
			t.Errorf("ran %q, out %q", ran, out.String())
		}
		failing := func(context.Context, string, ...string) error { return errors.New("exit status 1") }
		err := Update(context.Background(), Options{Method: tc.method, Out: &strings.Builder{}, Run: failing})
		if err == nil || !strings.HasSuffix(err.Error(), "\nFix: run "+tc.want+" yourself and read its output") {
			t.Errorf("err = %v", err)
		}
	}
}

func TestDetect(t *testing.T) {
	gobin := t.TempDir()
	goInstalled := &debug.BuildInfo{Path: MainPackage, Main: debug.Module{Path: "github.com/pilef-co/bqto", Version: "v0.11.0"}}
	dirs := func() []string { return []string{"", gobin} }
	tests := []struct {
		name string
		in   Install
		want Method
	}{
		{"Homebrew on Apple silicon", Install{Exe: "/opt/homebrew/Cellar/bqto/0.11.0/bin/bqto", AllowTools: true}, Homebrew},
		{"Homebrew on Linux", Install{Exe: "/home/linuxbrew/.linuxbrew/Cellar/bqto/0.11.0/bin/bqto", AllowTools: true}, Homebrew},
		{"Homebrew cask on Apple silicon", Install{Exe: "/opt/homebrew/Caskroom/bqto/0.15.0/bqto", AllowTools: true}, Homebrew},
		{"Homebrew cask on Linux", Install{Exe: "/home/linuxbrew/.linuxbrew/Caskroom/bqto/0.15.0/bqto", AllowTools: true}, Homebrew},
		{"another cask", Install{Exe: "/opt/homebrew/Caskroom/bqtool/1/bqto", Build: goInstalled, GoBinDirs: dirs, AllowTools: true}, Binary},
		{"another formula's Cellar", Install{Exe: "/opt/homebrew/Cellar/bqtool/1/bin/bqto", Build: goInstalled, GoBinDirs: dirs, AllowTools: true}, Binary},
		{"go install", Install{Exe: filepath.Join(gobin, "bqto"), Build: goInstalled, GoBinDirs: dirs, AllowTools: true}, GoInstall},
		{"go-installed binary moved elsewhere", Install{Exe: "/usr/local/bin/bqto", Build: goInstalled, GoBinDirs: dirs, AllowTools: true}, Binary},
		{"release binary in GOBIN", Install{Exe: filepath.Join(gobin, "bqto"), Build: goInstalled, Stamped: true, GoBinDirs: dirs, AllowTools: true}, Binary},
		{"go build from a checkout", Install{Exe: filepath.Join(gobin, "bqto"), Build: &debug.BuildInfo{Path: MainPackage, Main: debug.Module{Version: "(devel)"}}, GoBinDirs: dirs, AllowTools: true}, Binary},
		{"another main package", Install{Exe: filepath.Join(gobin, "bqto"), Build: &debug.BuildInfo{Path: "example.com/other/cmd/bqto", Main: debug.Module{Version: "v0.8.0"}}, GoBinDirs: dirs, AllowTools: true}, Binary},
		{"a custom updater never hands over to a tool", Install{Exe: "/opt/homebrew/Cellar/bqto/0.11.0/bin/bqto"}, Binary},
		{"plain binary", Install{Exe: "/usr/local/bin/bqto", AllowTools: true}, Binary},
	}
	for _, tc := range tests {
		if got := Detect(tc.in); got != tc.want {
			t.Errorf("%s: Detect() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
