package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilef-co/bqto/ext"
)

func TestBuildArgv(t *testing.T) {
	fwds := []Forward{
		{LocalPort: 15432, RemoteHost: "10.0.0.1", RemotePort: 5432},
		{LocalPort: 16379, RemoteHost: "10.0.0.2", RemotePort: 6379},
	}
	got := buildArgv("my-project", "us-central1-a", "my-vm", fwds)
	want := []string{
		"gcloud", "compute", "ssh", "my-vm",
		"--project", "my-project",
		"--zone", "us-central1-a",
		"--tunnel-through-iap",
		"--quiet",
		"--",
		"-N",
		"-v",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-L", "127.0.0.1:15432:10.0.0.1:5432",
		"-L", "127.0.0.1:16379:10.0.0.2:6379",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildArgvDynamicSOCKS(t *testing.T) {

	fwds := []Forward{{LocalPort: 1080}}
	got := buildArgv("my-project", "us-central1-a", "my-vm", fwds)
	want := []string{
		"gcloud", "compute", "ssh", "my-vm",
		"--project", "my-project",
		"--zone", "us-central1-a",
		"--tunnel-through-iap",
		"--quiet",
		"--",
		"-N",
		"-v",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-D", "127.0.0.1:1080",
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildArgvNoForwards(t *testing.T) {
	got := buildArgv("p", "z", "n", nil)

	fixed := 17
	if len(got) != fixed {
		t.Errorf("len = %d, want %d; got %v", len(got), fixed, got)
	}
}

func TestFreePort(t *testing.T) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port

	got, err := FreePort(occupied)
	if err != nil {
		t.Fatalf("FreePort(%d): %v", occupied, err)
	}
	if got == occupied {
		t.Errorf("FreePort returned the occupied port %d", occupied)
	}

	check, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", got))
	if err != nil {
		t.Errorf("port %d returned by FreePort is not free: %v", got, err)
	} else {
		check.Close()
	}

	got2, err := FreePort(got)
	if err != nil {
		t.Fatalf("FreePort(%d): %v", got, err)
	}
	if got2 != got {
		t.Errorf("FreePort(%d) = %d, want %d (port should be free)", got, got2, got)
	}
}

func TestIsPermanentTunnelError(t *testing.T) {
	tests := []struct {
		desc  string
		lines []string
		want  bool
	}{
		{"empty", nil, false},
		{"unrelated errors", []string{"connection refused", "4047 lookup failed"}, false},
		{"4033 code", []string{"Error 4033: Not authorized to tunnel"}, true},
		{"publickey denied", []string{"Permission denied (publickey,keyboard-interactive)."}, true},
		{"4033 buried in multiline", []string{"some line", "ERROR: exited [255]", "4033"}, true},
		{"publickey buried", []string{"debug1: connect to localhost", "Permission denied (publickey"}, true},
		{"4033 inside an instance ID", []string{"debug1: loaded identity cert from /home/u/.ssh/google_compute_engine_cert/p_us-central1-a_1111111111140331111-cert.pub", "Error while connecting [4047: 'Failed to lookup instance']."}, false},
		{"gcloud too old for certificates", []string{"ERROR: (gcloud.compute.ssh) SSH using certificates is not yet generally available (GA)."}, true},
		{"local port taken", []string{"bind [127.0.0.1]:1080: Address already in use"}, true},
		{"ssh refuses to bind", []string{"Could not request local forwarding.", "Cannot bind port 1080"}, true},
	}
	for _, tc := range tests {
		got := isPermanentTunnelError(tc.lines)
		if got != tc.want {
			t.Errorf("isPermanentTunnelError(%s): got %v, want %v", tc.desc, got, tc.want)
		}
	}
}

func fakeSignals(t *testing.T, onTerminate func()) (terms, kills *atomic.Int32) {
	t.Helper()
	terms, kills = new(atomic.Int32), new(atomic.Int32)
	origTerm, origKill := terminateTree, killTree
	t.Cleanup(func() { terminateTree, killTree = origTerm, origKill })
	terminateTree = func(*exec.Cmd) error {
		terms.Add(1)
		if onTerminate != nil {
			onTerminate()
		}
		return nil
	}
	killTree = func(*exec.Cmd) error { kills.Add(1); return nil }
	return terms, kills
}

func selfCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return &exec.Cmd{Process: p}
}

func TestStop(t *testing.T) {
	tests := []struct {
		desc           string
		exitedBefore   bool
		exitsOnTerm    bool
		wantTerms      int32
		wantKills      int32
		wantFasterThan time.Duration
	}{
		{desc: "already exited: no signal", exitedBefore: true, wantFasterThan: 100 * time.Millisecond},
		{desc: "exits on terminate: no kill", exitsOnTerm: true, wantTerms: 1, wantFasterThan: stopGrace},
		{desc: "ignores terminate: killed after the grace period", wantTerms: 1, wantKills: 1, wantFasterThan: stopGrace + time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			tun := &gcloudTunnel{doneC: make(chan struct{}), cmd: selfCmd(t)}
			if tc.exitedBefore {
				close(tun.doneC)
			}
			var onTerm func()
			if tc.exitsOnTerm {
				onTerm = func() { close(tun.doneC) }
			}
			terms, kills := fakeSignals(t, onTerm)

			start := time.Now()
			if err := tun.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if elapsed := time.Since(start); elapsed >= tc.wantFasterThan {
				t.Errorf("Stop took %v, want < %v", elapsed, tc.wantFasterThan)
			}
			if got := terms.Load(); got != tc.wantTerms {
				t.Errorf("terminateTree calls = %d, want %d", got, tc.wantTerms)
			}
			if got := kills.Load(); got != tc.wantKills {
				t.Errorf("killTree calls = %d, want %d", got, tc.wantKills)
			}
		})
	}
}

func TestStopTwiceSignalsOnce(t *testing.T) {
	tun := &gcloudTunnel{doneC: make(chan struct{}), cmd: selfCmd(t)}
	terms, _ := fakeSignals(t, func() { close(tun.doneC) })
	_ = tun.Stop()
	_ = tun.Stop()
	if got := terms.Load(); got != 1 {
		t.Errorf("terminateTree calls = %d, want 1", got)
	}
}

func TestNotReady(t *testing.T) {
	tests := []struct {
		desc   string
		ctxErr error
		lines  []string
		want   string
	}{
		{desc: "interrupted, no stderr", ctxErr: context.Canceled, want: "cancelled before the tunnel was ready"},
		{desc: "timed out, no stderr", ctxErr: context.DeadlineExceeded, want: "the tunnel was not ready in time\nFix: run bqto again; docs/troubleshooting.md lists the usual causes"},
		{
			desc: "timed out on a known failure", ctxErr: context.DeadlineExceeded,
			lines: []string{"ERROR: (gcloud.compute.start-iap-tunnel) Error while connecting [4003: 'failed to connect to backend']."},
			want:  "the tunnel was not ready in time: IAP could not reach port 22 on the bastion (4003)\nFix: allow ingress on tcp:22 from 35.235.240.0/20 to the bastion",
		},
		{
			desc: "timed out on an unknown failure", ctxErr: context.DeadlineExceeded,
			lines: []string{"ssh: connect to host compute.123 port 22: Connection refused"},
			want:  "the tunnel was not ready in time\nlast stderr:\nssh: connect to host compute.123 port 22: Connection refused\nFix: " + troubleshootingFix,
		},
		{
			desc: "timed out with gcloud's advice", ctxErr: context.DeadlineExceeded,
			lines: []string{
				"debug: something",
				"Recommendation: To check for possible causes of SSH connectivity issues and get",
				"recommendations, rerun the ssh command with the --troubleshoot option.",
				"",
				"gcloud compute ssh bqto-x --project=p --zone=z --troubleshoot --tunnel-through-iap",
				"",
				"ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255].",
			},
			want: "the tunnel was not ready in time\nlast stderr:\nERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255].\nFix: " + troubleshootingFix,
		},
		{
			desc: "timed out with only advice", ctxErr: context.DeadlineExceeded,
			lines: []string{"", "Or, to investigate an IAP tunneling issue:", "gcloud compute ssh bqto-x --troubleshoot"},
			want:  "the tunnel was not ready in time\nFix: run bqto again; docs/troubleshooting.md lists the usual causes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := notReady(tc.ctxErr, tc.lines).Error()
			if got != tc.want {
				t.Errorf("notReady() =\n%s\nwant\n%s", got, tc.want)
			}
			if strings.HasPrefix(got, "tunnel:") {
				t.Errorf("notReady() = %q, want no tunnel: prefix", got)
			}
		})
	}
}

func TestExplain(t *testing.T) {
	tests := []struct {
		line   string
		want   string
		wantOK bool
	}{
		{line: "Error while connecting [4033: 'not authorized'].", want: "IAP refused the tunnel (4033)", wantOK: true},
		{line: "Error while connecting [4003: 'failed to connect to backend'].", want: "IAP could not reach port 22 on the bastion (4003)", wantOK: true},
		{line: "Error while connecting [4047: 'Failed to lookup instance'].", want: "IAP could not find the bastion (4047)", wantOK: true},
		{line: "user@compute.123: Permission denied (publickey).", want: "OS Login refused the SSH certificate", wantOK: true},
		{line: "ERROR: (gcloud.compute.ssh) SSH using certificates is not yet generally available (GA). Please use `gcloud beta compute ssh`", want: "this gcloud is too old for OS Login certificates", wantOK: true},
		{line: "Connection closed by remote host"},
		{line: "debug1: loaded identity cert from /home/u/.ssh/google_compute_engine_cert/p_us-central1-a_1111111111140331111-cert.pub: RSA-CERT"},
		{line: "Authenticated to compute.1114003111114047111 (via proxy) using \"publickey\"."},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got, ok := Explain([]string{"Testing if tunnel connection works.", tc.line})
			if ok != tc.wantOK || !strings.HasPrefix(got, tc.want) {
				t.Errorf("Explain() = %q, %v; want prefix %q, %v", got, ok, tc.want, tc.wantOK)
			}
			if ok && !strings.Contains(got, "\nFix: ") {
				t.Errorf("Explain() = %q, want a Fix line", got)
			}
		})
	}
}

func TestStartRejectsSomebodyElsesListener(t *testing.T) {

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	tn := &gcloudTunnel{
		argv:  []string{"false"},
		fwds:  []Forward{{LocalPort: port, RemoteHost: "10.0.0.1", RemotePort: 5432}},
		doneC: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := tn.Start(ctx); err == nil {
		t.Fatal("Start returned nil although the tunnel process had exited")
	}
	if closed(tn.doneC) {
		t.Error("doneC was closed for a tunnel that was never ready")
	}
}

func TestExplainBindFailure(t *testing.T) {
	cause, ok := Explain([]string{"bind [127.0.0.1]:1080: Address already in use"})
	if !ok {
		t.Fatal("Explain did not recognise the bind failure")
	}
	if !strings.Contains(cause, "loopback port") || strings.Contains(cause, "-p") {
		t.Errorf("cause = %q, want the tunnel's loopback port named and no -p advice: bqto holds the session port", cause)
	}
}

func TestBoundPorts(t *testing.T) {
	fwds := []Forward{{LocalPort: 15432, RemoteHost: "10.0.0.5", RemotePort: 5432}, {LocalPort: 16379, RemoteHost: "10.0.0.6", RemotePort: 6379}}
	tests := []struct {
		desc  string
		lines []string
		fwds  []Forward
		want  bool
	}{
		{desc: "nothing yet", fwds: fwds},
		{
			desc:  "only one of the two bound",
			lines: []string{"debug1: Local forwarding listening on 127.0.0.1 port 15432."},
			fwds:  fwds,
		},
		{
			desc: "both bound",
			lines: []string{
				"debug1: Local connections to 127.0.0.1:15432 forwarded to remote address 10.0.0.5:5432",
				"debug1: Local forwarding listening on 127.0.0.1 port 15432.",
				"debug1: Local forwarding listening on 127.0.0.1 port 16379.",
			},
			fwds: fwds,
			want: true,
		},
		{
			desc:  "another session's port does not count",
			lines: []string{"debug1: Local forwarding listening on 127.0.0.1 port 15433."},
			fwds:  fwds[:1],
		},
		{
			desc:  "a SOCKS forward",
			lines: []string{"debug1: Local forwarding listening on 127.0.0.1 port 1080."},
			fwds:  []Forward{{LocalPort: 1080}},
			want:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			r := &stderrRing{max: 60}
			for _, l := range tc.lines {
				fmt.Fprintln(r, l)
			}
			if got := r.boundAll(tc.fwds); got != tc.want {
				t.Errorf("boundAll = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadyMarkerSurvivesALongDebugLog(t *testing.T) {
	r := &stderrRing{max: 60}
	fmt.Fprintln(r, "debug1: Local forwarding listening on 127.0.0.1 port 15432.")
	for i := range 100 {
		fmt.Fprintf(r, "debug1: channel %d: new [port listener]\n", i)
	}
	if len(r.lines()) != 60 {
		t.Fatalf("the ring keeps %d lines, want 60", len(r.lines()))
	}
	if !r.boundAll([]Forward{{LocalPort: 15432}}) {
		t.Error("the port counts as not bound once its line left the ring")
	}
}

func TestStopAfterAFailedStartSendsNoSignal(t *testing.T) {
	tun := Gcloud("p", "z", "bqto-x", []Forward{{LocalPort: 1}}).(*gcloudTunnel)
	tun.argv = []string{"sh", "-c", "echo 'ERROR: 4033 not authorized' >&2; exit 255"}
	terms, kills := fakeSignals(t, nil)
	if err := tun.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded, want the 4033 failure")
	}
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_ = tun.Stop()
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Errorf("Stop took %v, want < 100ms", elapsed)
	}
	if terms.Load() != 0 || kills.Load() != 0 {
		t.Errorf("signals sent to a process that already exited: %d terminate, %d kill", terms.Load(), kills.Load())
	}
}

func TestWithoutDebug(t *testing.T) {
	got := WithoutDebug([]string{"debug1: noise", "ERROR: 4047 lookup failed", "debug2: more noise"})
	if len(got) != 1 || got[0] != "ERROR: 4047 lookup failed" {
		t.Errorf("withoutDebug = %v, want only the real error", got)
	}
}

func TestStartReportsPortTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	tn := &gcloudTunnel{
		argv:  []string{"sh", "-c", "echo 'bind [127.0.0.1]:" + fmt.Sprint(port) + ": Address already in use' >&2; exit 255"},
		fwds:  []Forward{{LocalPort: port, RemoteHost: "10.0.0.1", RemotePort: 5432}},
		doneC: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	startErr := tn.Start(ctx)
	if !errors.Is(startErr, ErrPortTaken) {
		t.Fatalf("Start error = %v, want ErrPortTaken", startErr)
	}
	if !errors.Is(startErr, ErrPermanent) {
		t.Errorf("a taken port must also be permanent: %v", startErr)
	}
	want := fmt.Sprintf("another program took the tunnel's loopback port while the tunnel started: port %d\nFix: run bqto connect again", port)
	if startErr.Error() != want {
		t.Errorf("Start error = %q, want %q", startErr, want)
	}
	if strings.Contains(startErr.Error(), "permanent") || strings.Contains(startErr.Error(), "retry") {
		t.Errorf("the retry marker leaks into the message: %q", startErr)
	}
}

func TestStderrKeepsTheFailedAttempt(t *testing.T) {
	tn := &gcloudTunnel{
		argv:  []string{"sh", "-c", "echo 'debug1: noise' >&2; echo 'ERROR: 4033 not authorized' >&2; exit 255"},
		fwds:  []Forward{{LocalPort: 65535, RemoteHost: "10.0.0.1", RemotePort: 5432}},
		doneC: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tn.Start(ctx); err == nil {
		t.Fatal("Start returned nil for a failing command")
	}
	lines := tn.Stderr()
	if len(lines) != 1 || !strings.Contains(lines[0], "4033") {
		t.Errorf("Stderr = %v, want the failed attempt without ssh's debug lines", lines)
	}
}

func TestBootRace(t *testing.T) {
	denied := []string{"debug1: Next authentication method: publickey", "user@compute.1: Permission denied (publickey)."}
	tests := []struct {
		desc  string
		lines []string
		want  bool
	}{
		{"publickey denied", denied, true},
		{"4033 is never a race", append([]string{"Error 4033: not authorized"}, denied...), false},
		{"gcloud too old is never a race", append([]string{"ERROR: (gcloud.compute.ssh) " + certificatesNotGA}, denied...), false},
		{"no denial", []string{"4003: failed to connect to backend"}, false},
	}
	for _, tc := range tests {
		if got := bootRace(tc.lines); got != tc.want {
			t.Errorf("bootRace(%s) = %v, want %v", tc.desc, got, tc.want)
		}
	}
}

func publickeyTunnel(t *testing.T) (*gcloudTunnel, string) {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	return &gcloudTunnel{
		argv:  []string{"sh", "-c", "echo x >> '" + count + "'; echo 'user@compute.1: Permission denied (publickey).' >&2; exit 255"},
		fwds:  []Forward{{LocalPort: 65535, RemoteHost: "10.0.0.1", RemotePort: 5432}},
		doneC: make(chan struct{}),
	}, count
}

func TestStartRetriesPublickeyDuringBoot(t *testing.T) {
	oldDelay := retryDelay
	retryDelay = 10 * time.Millisecond
	t.Cleanup(func() { retryDelay = oldDelay })
	var log strings.Builder
	ext.SetDebug(&log)
	t.Cleanup(func() { ext.SetDebug(nil) })

	tn, count := publickeyTunnel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := tn.Start(ctx)
	ext.SetDebug(nil)
	if errors.Is(err, ErrPermanent) {
		t.Fatalf("a publickey denial inside the grace must be retried: %v", err)
	}
	data, _ := os.ReadFile(count)
	if n := strings.Count(string(data), "x"); n < 2 {
		t.Errorf("gcloud ran %d times, want a retry", n)
	}
	lines := strings.Split(log.String(), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "debug: gcloud: sh -c ") {
		t.Fatalf("debug log = %q, want the gcloud line then a retry", lines)
	}
	if want := "debug: tunnel: attempt 2, after: user@compute.1: Permission denied (publickey)."; lines[1] != want {
		t.Errorf("second debug line = %q, want %q", lines[1], want)
	}
	if strings.Contains(log.String(), "attempt 1,") {
		t.Errorf("the first attempt is not a retry: %q", log.String())
	}
}

func TestStartGivesUpOnPublickeyAfterTheGrace(t *testing.T) {
	old := publickeyGrace
	publickeyGrace = 0
	defer func() { publickeyGrace = old }()

	tn, _ := publickeyTunnel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := tn.Start(ctx)
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("Start error = %v, want ErrPermanent", err)
	}
	if !strings.Contains(err.Error(), "OS Login refused the SSH certificate") {
		t.Errorf("Start error = %q, want the OS Login explanation", err)
	}
}

func TestStartIgnoresCodesInDebugLines(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	tn := &gcloudTunnel{
		argv:  []string{"sh", "-c", "echo x >> '" + count + "'; echo 'debug1: Transferred: sent 4033, received 4003 bytes' >&2; exit 255"},
		fwds:  []Forward{{LocalPort: 65535, RemoteHost: "10.0.0.1", RemotePort: 5432}},
		doneC: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := tn.Start(ctx); errors.Is(err, ErrPermanent) {
		t.Fatalf("a code inside a debug line must not end the start: %v", err)
	}
	data, _ := os.ReadFile(count)
	if n := strings.Count(string(data), "x"); n < 2 {
		t.Errorf("gcloud ran %d times, want a retry", n)
	}
}

func TestFixesNameProFeaturesOnlyInTheProBinary(t *testing.T) {
	denied := []string{"user@compute.1: Permission denied (publickey)."}
	for _, tc := range []struct {
		pro                     bool
		port, osLoginCause, fix string
	}{
		{false, "free it, or pick another port with -p", "OS Login refused the SSH certificate: your account lacks roles/compute.osLogin", "\nFix: grant roles/compute.osLogin on the project"},
		{true, "free it, or pick another port with -p (in a stack: the profile's local_port, bqto profile create NAME ... -p PORT)", "OS Login refused the SSH certificate: either your account lacks roles/compute.osLogin, or the bastion's image", "; with --image, use an image that supports OS Login certificates, such as the default Debian"},
	} {
		free := proFlag
		proFlag = func(string) bool { return tc.pro }
		if got := PortTakenFix(); got != tc.port {
			t.Errorf("pro=%v: PortTakenFix() = %q, want %q", tc.pro, got, tc.port)
		}
		got, ok := Explain(denied)
		if !ok || !strings.HasPrefix(got, tc.osLoginCause) || !strings.HasSuffix(got, tc.fix) {
			t.Errorf("pro=%v: Explain = %q, want %q ... %q", tc.pro, got, tc.osLoginCause, tc.fix)
		}
		proFlag = free
	}
}

func TestRetryReason(t *testing.T) {
	exited := exec.Command("sh", "-c", "exit 255")
	_ = exited.Run()
	tests := []struct {
		desc  string
		lines []string
		exit  *os.ProcessState
		want  string
	}{
		{"one gcloud line", []string{"ERROR: 4003: failed to connect to backend"}, exited.ProcessState, "ERROR: 4003: failed to connect to backend"},
		{"last three gcloud lines", []string{"a", "b", "c", "d"}, exited.ProcessState, "b | c | d"},
		{"exit status without output", nil, exited.ProcessState, "gcloud exit status 255 with no output"},
		{"blank lines and gcloud's --troubleshoot advice", []string{
			"Recommendation: To check for possible causes of SSH connectivity issues and get",
			"recommendations, rerun the ssh command with the --troubleshoot option.",
			"",
			"gcloud compute ssh bqto-x --project=p --zone=z --troubleshoot",
			"",
			"Or, to investigate an IAP tunneling issue:",
			"",
			"gcloud compute ssh bqto-x --project=p --zone=z --troubleshoot --tunnel-through-iap",
			"",
			"ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255].",
		}, exited.ProcessState, "ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255]."},
		{"the ssh cause before the advice", []string{
			"kex_exchange_identification: Connection closed by remote host",
			"Connection closed by UNKNOWN port 65535",
			"   ",
			"gcloud compute ssh bqto-x --project=p --zone=z --troubleshoot --tunnel-through-iap",
			"ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255].",
		}, exited.ProcessState, "kex_exchange_identification: Connection closed by remote host | Connection closed by UNKNOWN port 65535 | ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255]."},
		{"only blank lines", []string{"", "  "}, exited.ProcessState, "gcloud exit status 255 with no output"},
	}
	for _, tc := range tests {
		if got := retryReason(tc.lines, tc.exit); got != tc.want {
			t.Errorf("retryReason(%s) = %q, want %q", tc.desc, got, tc.want)
		}
	}
}
