package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCleanupMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		desc      string
		mayExist  bool
		requested bool
		err       error
		want      string
	}{
		{desc: "delete accepted", requested: true, want: "bastion bqto-1 is being deleted (about 2 minutes)"},
		{desc: "vm never created", want: ""},
		{desc: "interrupted create left no vm", mayExist: true, want: "no bastion was created"},
		{desc: "delete failed", err: errors.New("delete vm: boom"), want: "error: bastion bqto-1 may still be running: delete vm: boom\nFix: gcloud compute instances list --project p --filter=labels.bqto=session, then delete it"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := cleanupMessage("p", "bqto-1", tc.mayExist, tc.requested, tc.err); got != tc.want {
				t.Errorf("cleanupMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCleanupStartMessage(t *testing.T) {
	tests := []struct {
		desc     string
		mayExist bool
		want     string
	}{
		{desc: "create finished", want: "deleting bastion…"},
		{desc: "create interrupted", mayExist: true, want: "checking whether the bastion was created…"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			if got := cleanupStartMessage(tc.mayExist); got != tc.want {
				t.Errorf("cleanupStartMessage(%v) = %q, want %q", tc.mayExist, got, tc.want)
			}
		})
	}
}

func TestWarnDelay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		desc     string
		deadline time.Time
		want     time.Duration
		wantOK   bool
	}{
		{desc: "30-minute session", deadline: now.Add(30 * time.Minute), want: 25 * time.Minute, wantOK: true},
		{desc: "6-minute session", deadline: now.Add(6 * time.Minute), want: time.Minute, wantOK: true},
		{desc: "5-minute session: no warning", deadline: now.Add(5 * time.Minute)},
		{desc: "1-minute session: no warning", deadline: now.Add(time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got, ok := warnDelay(now, tc.deadline)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("warnDelay() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestEndReason(t *testing.T) {
	t.Parallel()
	deadline := time.Date(2026, 9, 22, 12, 30, 0, 0, time.UTC)
	early := deadline.Add(-10 * time.Minute)
	late := deadline.Add(10 * time.Second)
	tests := []struct {
		desc        string
		statusErr   error
		found       bool
		status      string
		spot        bool
		now         time.Time
		stderr      []string
		lastStart   error
		want        string
		wantExpired bool
	}{
		{desc: "deleted at the deadline", now: late, want: "the bastion reached its max duration (30m)", wantExpired: true},
		{desc: "stopping just before our deadline", found: true, status: "STOPPING", now: deadline.Add(-30 * time.Second), want: "the bastion reached its max duration (30m)", wantExpired: true},
		{desc: "spot deleted early", spot: true, now: early, want: "the Spot bastion was preempted"},
		{desc: "on-demand deleted early", now: early, want: "the bastion was stopped or deleted before its deadline"},
		{desc: "still running", found: true, status: "RUNNING", now: early, want: "the tunnel kept dropping and bqto stopped reconnecting, although the bastion was still running"},
		{
			desc: "still running, known failure", found: true, status: "RUNNING", now: early,
			stderr: []string{"Error while connecting [4003: 'failed to connect to backend']."},
			want:   "the tunnel dropped while the bastion was still running: IAP could not reach port 22 on the bastion (4003)\nFix:",
		},
		{
			desc: "still running, the last reconnect failed", found: true, status: "RUNNING", now: early,
			lastStart: errors.New("the tunnel was not ready in time"),
			want:      "the tunnel kept dropping and bqto stopped reconnecting, although the bastion was still running (the laptop slept, the network changed, or IAP closed the connection); the last attempt: the tunnel was not ready in time\nFix: run bqto connect again",
		},
		{
			desc: "still running, an unknown error on stderr", found: true, status: "RUNNING", now: early,
			stderr: []string{"debug1: noise", "ERROR: something new"},
			want:   "the tunnel kept dropping and bqto stopped reconnecting, although the bastion was still running (the laptop slept, the network changed, or IAP closed the connection)\nlast stderr:\nERROR: something new\nFix: run bqto connect again",
		},
		{
			desc: "still running, gcloud's advice on stderr", found: true, status: "RUNNING", now: early,
			stderr: []string{"ERROR: something new", "", "gcloud compute ssh bqto-x --troubleshoot --tunnel-through-iap", ""},
			want:   "the tunnel kept dropping and bqto stopped reconnecting, although the bastion was still running (the laptop slept, the network changed, or IAP closed the connection)\nlast stderr:\nERROR: something new\nFix: run bqto connect again",
		},
		{
			desc: "still running, the last reconnect found the port taken", found: true, status: "RUNNING", now: early,
			lastStart: fmt.Errorf("%w\nFix: run bqto connect again", errInnerPortTaken),
			want:      "the tunnel kept dropping and bqto stopped reconnecting, although the bastion was still running (the laptop slept, the network changed, or IAP closed the connection); the last attempt: another program took the tunnel's loopback port while the tunnel started\nFix: run bqto connect again",
		},
		{desc: "status unknown", statusErr: errors.New("get vm: 403"), now: early, want: "the tunnel exited, and bqto could not check the bastion: get vm: 403\nFix: run bqto connect again"},
		{desc: "status unknown, with its own fix", statusErr: errors.New("get vm: 403\nFix: gcloud auth application-default login"), now: early, want: "the tunnel exited, and bqto could not check the bastion: get vm: 403\nFix: gcloud auth application-default login"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := endReason(tc.statusErr, tc.found, tc.status, tc.spot, tc.now, deadline, "30m", tc.stderr, tc.lastStart)
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("endReason() = %q, want prefix %q", err, tc.want)
			}
			var expired sessionExpired
			if got := errors.As(err, &expired); got != tc.wantExpired {
				t.Errorf("expired = %v, want %v", got, tc.wantExpired)
			}
		})
	}
}

func TestTimeLeft(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 13, 49, 0, 0, time.UTC)
	tests := []struct {
		left time.Duration
		want string
	}{
		{left: 4*time.Minute + 10*time.Second, want: "in 4m"},
		{left: 30 * time.Minute, want: "in 30m"},
		{left: time.Hour, want: "in 1h"},
		{left: 90 * time.Minute, want: "in 1h30m"},
		{left: 20 * time.Second, want: "in less than a minute"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := timeLeft(now, now.Add(tc.left)); got != tc.want {
				t.Errorf("timeLeft(%v) = %q, want %q", tc.left, got, tc.want)
			}
		})
	}
}

func TestCanReconnect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		desc      string
		statusErr error
		found     bool
		status    string
		deadline  time.Time
		want      bool
	}{
		{desc: "running with time left", found: true, status: "RUNNING", deadline: now.Add(30 * time.Minute), want: true},
		{desc: "deleted", found: false, status: "", deadline: now.Add(30 * time.Minute)},
		{desc: "terminated", found: true, status: "TERMINATED", deadline: now.Add(30 * time.Minute)},
		{desc: "stopping", found: true, status: "STOPPING", deadline: now.Add(30 * time.Minute)},
		{desc: "status unknown", statusErr: errors.New("boom"), found: true, status: "RUNNING", deadline: now.Add(30 * time.Minute)},
		{desc: "about to expire", found: true, status: "RUNNING", deadline: now.Add(30 * time.Second)},
		{desc: "past the deadline", found: true, status: "RUNNING", deadline: now.Add(-time.Minute)},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := canReconnect(c.statusErr, c.found, c.status, now, c.deadline); got != c.want {
				t.Errorf("canReconnect = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSessionSignalsIncludeHangup(t *testing.T) {
	for _, want := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !slices.Contains(sessionSignals, want) {
			t.Errorf("sessionSignals lacks %v: closing the terminal or a kill would skip the cleanup", want)
		}
	}
}

func TestInterruptedKeepsTheCommandsCode(t *testing.T) {
	cancelled := context.Canceled
	tests := []struct {
		desc        string
		err, ctxErr error
		wantCode    int
	}{
		{desc: "no error", ctxErr: cancelled},
		{desc: "an error after Ctrl-C", err: errors.New("create bastion: cancelled"), ctxErr: cancelled, wantCode: exitInterrupted},
		{desc: "the command's own code after Ctrl-C", err: exitCodeError{code: 3}, ctxErr: cancelled, wantCode: 3},
		{desc: "a failed delete after Ctrl-C", err: exitCodeError{code: exitError}, ctxErr: cancelled, wantCode: exitError},
		{desc: "an error with no signal", err: errors.New("boom"), wantCode: exitError},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			err := interrupted(tc.err, tc.ctxErr)
			if tc.wantCode == 0 {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if got := report(io.Discard, err); got != tc.wantCode {
				t.Errorf("exit code = %d, want %d", got, tc.wantCode)
			}
		})
	}
}

func TestRunCommandForwardsASignalSentToBqtoAlone(t *testing.T) {
	tun := &fakeTunnel{done: make(chan struct{})}
	cfg := connectConfig{Service: "redis", Cmd: []string{"sh", "-c", "trap 'exit 7' INT; while :; do sleep 0.05; done"}}
	result := make(chan error, 1)
	go func() { result <- runCommand(cfg, nil, tun, func() error { return nil }, nil) }()
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != 7 {
			t.Errorf("err = %v, want the command's exit code 7", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not get the SIGINT sent to bqto")
	}
}

func TestFindCommand(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		cmd      []string
		wantCode int
	}{
		{nil, 0},
		{[]string{"sh", "-c", "true"}, 0},
		{[]string{"bqto-no-such-command"}, exitCmdNotFound},
		{[]string{"./bqto-no-such-script.sh"}, exitCmdNotFound},
		{[]string{notExecutable}, exitCmdNotRunnable},
		{[]string{dir}, exitCmdNotRunnable},
	}
	for _, tc := range tests {
		err := findCommand(tc.cmd)
		var ec exitCodeError
		switch {
		case tc.wantCode == 0 && err != nil:
			t.Errorf("findCommand(%q) = %v, want nil", tc.cmd, err)
		case tc.wantCode != 0 && (!errors.As(err, &ec) || ec.code != tc.wantCode):
			t.Errorf("findCommand(%q) = %v, want exit code %d", tc.cmd, err, tc.wantCode)
		}
	}
}

func TestDoConnectStopsOnAMissingCommandBeforeAuth(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/bqto-no-such-credentials.json")
	cfg := connectConfig{Project: "p", Service: "cloudsql", Instance: "sql", Duration: "30m", NoInput: true, Cmd: []string{"bqto-no-such-command"}}
	err := doConnect([]connectConfig{cfg}, map[string]bool{})
	var ec exitCodeError
	if !errors.As(err, &ec) || ec.code != exitCmdNotFound {
		t.Errorf("err = %v, want exit code %d before auth is checked", err, exitCmdNotFound)
	}
}

func TestCleanupMessageHasOneFix(t *testing.T) {
	err := errors.New("delete bastion bqto-1: Required 'compute.instances.delete' permission\nFix: ask for a role that grants compute.instances.delete")
	msg := cleanupMessage("p", "bqto-1", false, false, err)
	if strings.Count(msg, "Fix:") != 1 || !strings.Contains(msg, "compute.instances.delete' permission") {
		t.Errorf("cleanupMessage = %q, want the cause and one Fix line", msg)
	}
}

func TestVMIDWarningDropsTheFix(t *testing.T) {
	err := errors.New("get bastion bqto-x: permission denied\nFix: grant roles/compute.viewer")
	want := "warn: get bastion bqto-x: permission denied; its host key and certificate stay in ~/.ssh"
	if got := vmIDWarning(err); got != want {
		t.Errorf("vmIDWarning = %q, want %q", got, want)
	}
}
