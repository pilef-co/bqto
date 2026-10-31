package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/alloydb"
	"github.com/pilef-co/bqto/internal/cloudsql"
	"github.com/pilef-co/bqto/internal/redis"
)

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		ldflags, mod, want string
	}{
		{"v1.2.3", "v0.9.0", "v1.2.3"},
		{"dev", "v0.9.0", "v0.9.0"},
		{"dev", "(devel)", "dev"},
		{"dev", "", "dev"},
		{"", "", "dev"},
	}
	for _, tc := range tests {
		if got := resolveVersion(tc.ldflags, tc.mod); got != tc.want {
			t.Errorf("resolveVersion(%q, %q) = %q, want %q", tc.ldflags, tc.mod, got, tc.want)
		}
	}
}

func TestReportExitCodes(t *testing.T) {
	tests := []struct {
		desc     string
		err      error
		wantCode int
		wantOut  string
	}{
		{desc: "an error", err: errors.New("boom"), wantCode: exitError, wantOut: "error: boom\n"},
		{desc: "a usage error", err: usagef("bad flag"), wantCode: exitUsage, wantOut: "error: bad flag\n"},
		{desc: "a wrapped usage error", err: fmt.Errorf("connect: %w", usagef("bad flag")), wantCode: exitUsage, wantOut: "error: bad flag\n"},
		{desc: "the command after -- is missing", err: exitCodeError{code: exitCmdNotFound, err: exec.ErrNotFound}, wantCode: exitCmdNotFound, wantOut: "error: " + exec.ErrNotFound.Error() + "\n"},
		{desc: "the wizard was quit", err: fmt.Errorf("wizard: %w", huh.ErrUserAborted), wantCode: exitInterrupted},
		{desc: "Ctrl-C", err: exitCodeError{code: exitInterrupted}, wantCode: exitInterrupted},
		{desc: "the child's own code", err: exitCodeError{code: 3}, wantCode: 3},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			var out bytes.Buffer
			if got := report(&out, tc.err); got != tc.wantCode {
				t.Errorf("code = %d, want %d", got, tc.wantCode)
			}
			if out.String() != tc.wantOut {
				t.Errorf("printed %q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("%s counts as a terminal, so cron and CI would get the wizard", os.DevNull)
	}
}

func TestCommandEnv(t *testing.T) {
	tests := []struct {
		desc   string
		target target
		want   []string
		absent []string
	}{
		{desc: "cloudsql", target: goldenTarget("cloudsql", cloudsql.Target(cloudsql.Endpoint{IP: "10.0.0.3", Port: 5432}, "sql", "", "")), want: []string{"BQTO_HOST=127.0.0.1", "BQTO_PORT=15432", "PGHOST=127.0.0.1", "PGPORT=15432"}, absent: []string{"ALL_PROXY"}},
		{desc: "alloydb", target: goldenTarget("alloydb", alloydb.Target(alloydb.Endpoint{IP: "10.0.0.4", Port: 5432}, "r/c/i", "", "")), want: []string{"BQTO_PORT=15432", "PGPORT=15432"}},
		{desc: "redis", target: goldenTarget("redis", redis.Target(redis.Endpoint{IP: "10.0.0.5", Port: 6379}, "r/i", "", "")), want: []string{"BQTO_HOST=127.0.0.1", "BQTO_PORT=15432"}, absent: []string{"PGPORT", "ALL_PROXY"}},
		{desc: "a service with its own variables", target: socksTarget("r/thing"), want: []string{"BQTO_PORT=15432", "ALL_PROXY=socks5h://127.0.0.1:15432"}, absent: []string{"PGPORT"}},
	}
	for _, tc := range tests {
		env := strings.Join(commandEnv(tc.target, 15432), "\n")
		for _, w := range tc.want {
			if !strings.Contains(env, w) {
				t.Errorf("%s: env %q lacks %q", tc.desc, env, w)
			}
		}
		for _, a := range tc.absent {
			if strings.Contains(env, a) {
				t.Errorf("%s: env %q has %q", tc.desc, env, a)
			}
		}
	}
}

func TestDebugFlagAndEnvironmentTurnDebugOn(t *testing.T) {
	t.Cleanup(func() { ext.SetDebug(nil) })
	_ = cmdConnect([]string{"--debug", "--no-input"})
	if !ext.Debugging() {
		t.Error("--debug did not turn debug output on")
	}
	ext.SetDebug(nil)
	t.Setenv("BQTO_DEBUG", "1")
	_ = Main([]string{"version"})
	if !ext.Debugging() {
		t.Error("BQTO_DEBUG=1 did not turn debug output on")
	}
	for _, off := range []string{"0", "false"} {
		ext.SetDebug(nil)
		t.Setenv("BQTO_DEBUG", off)
		_ = Main([]string{"version"})
		if ext.Debugging() {
			t.Errorf("BQTO_DEBUG=%s turned debug output on", off)
		}
	}
	ext.SetDebug(os.Stderr)
	_ = cmdConnect([]string{"--debug=false", "--no-input"})
	if ext.Debugging() {
		t.Error("--debug=false left debug output on")
	}
}

func TestDebugBeforeTheCommand(t *testing.T) {
	t.Cleanup(func() { ext.SetDebug(nil) })
	t.Setenv("BQTO_DEBUG", "")
	for _, args := range [][]string{{"--debug", "version"}, {"-debug", "version"}, {"--debug=true", "version"}, {"--debug", "--debug=1", "version"}} {
		ext.SetDebug(nil)
		if err := run(args); err != nil {
			t.Errorf("run(%q) = %v", args, err)
		}
		if !ext.Debugging() {
			t.Errorf("run(%q) left debug output off", args)
		}
	}
	ext.SetDebug(os.Stderr)
	if err := run([]string{"--debug=false", "version"}); err != nil || ext.Debugging() {
		t.Errorf("run(--debug=false version) = %v, debugging %v", err, ext.Debugging())
	}
	err := run([]string{"--debug=maybe", "version"})
	if code := report(io.Discard, err); code != exitUsage || !strings.Contains(err.Error(), `invalid boolean value "maybe" for flag --debug`) {
		t.Errorf("run(--debug=maybe version) = %v, exit %d", err, code)
	}
	t.Setenv("BQTO_DEBUG", "1")
	if code := Main([]string{"--debug=false", "version"}); code != 0 || ext.Debugging() {
		t.Errorf("BQTO_DEBUG=1 bqto --debug=false version: exit %d, debugging %v", code, ext.Debugging())
	}
	if err := run([]string{"--debug", "nope"}); err == nil || !strings.Contains(err.Error(), `unknown command "nope"`) {
		t.Errorf("run(--debug nope) = %v, want unknown command", err)
	}
}
