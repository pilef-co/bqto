package tunnel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRetryWait(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},
		{9, 30 * time.Second},
	}
	for _, c := range cases {
		if got := retryWait(c.attempt); got != c.want {
			t.Errorf("retryWait(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

type fakeTunnel struct {
	startErr error
	mu       sync.Mutex
	doneC    chan struct{}
	stderr   []string
	stopped  bool
}

func newFake(stderr ...string) *fakeTunnel {
	return &fakeTunnel{doneC: make(chan struct{}), stderr: stderr}
}

func (f *fakeTunnel) Start(_ context.Context) error { return f.startErr }
func (f *fakeTunnel) Done() <-chan struct{}         { return f.doneC }
func (f *fakeTunnel) Stderr() []string              { return f.stderr }

func (f *fakeTunnel) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeTunnel) drop() { close(f.doneC) }

func (f *fakeTunnel) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func fastBackoff(int) time.Duration { return time.Millisecond }

func TestSuperviseReconnectsWhileAlive(t *testing.T) {
	first, second := newFake(), newFake()
	var made []*fakeTunnel
	var mu sync.Mutex
	up := make(chan struct{}, 1)

	sup := Supervisor{
		New: func() Tunnel {
			mu.Lock()
			defer mu.Unlock()
			n := first
			if len(made) > 0 {
				n = second
			}
			made = append(made, n)
			return n
		},
		Alive:   func() bool { return true },
		OnUp:    func() { up <- struct{}{} },
		Backoff: fastBackoff,
	}
	tun := sup.Tunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first.drop()
	select {
	case <-up:
	case <-time.After(2 * time.Second):
		t.Fatal("did not reconnect")
	}
	select {
	case <-tun.Done():
		t.Fatal("Done closed although the tunnel reconnected")
	default:
	}

	second.drop()
	_ = tun.Stop()
	<-tun.Done()
}

func TestSuperviseStopsWhenBastionIsGone(t *testing.T) {
	first := newFake("ERROR: 4047")
	calls := 0
	tun := Supervisor{
		New: func() Tunnel {
			calls++
			return first
		},
		Alive:   func() bool { return false },
		Backoff: fastBackoff,
	}.Tunnel()

	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first.drop()

	select {
	case <-tun.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed")
	}
	if calls != 1 {
		t.Errorf("New called %d times, want 1", calls)
	}
	if got := tun.Stderr(); len(got) != 1 || got[0] != "ERROR: 4047" {
		t.Errorf("Stderr = %v, want the last stderr of the dead tunnel", got)
	}
}

func TestSuperviseGivesUpAfterMaxReconnects(t *testing.T) {
	var made []*fakeTunnel
	var mu sync.Mutex
	tun := Supervisor{
		New: func() Tunnel {
			mu.Lock()
			defer mu.Unlock()
			f := newFake()
			if len(made) > 0 {
				f.startErr = errors.New("gcloud failed")
			}
			made = append(made, f)
			return f
		},
		Alive:   func() bool { return true },
		Backoff: fastBackoff,
	}.Tunnel()

	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mu.Lock()
	first := made[0]
	mu.Unlock()
	first.drop()

	select {
	case <-tun.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done was not closed after the retries ran out")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(made) != maxReconnects+1 {
		t.Errorf("New called %d times, want %d", len(made), maxReconnects+1)
	}
}

func TestSuperviseStopEndsTheSession(t *testing.T) {
	first := newFake()
	tun := Supervisor{
		New:     func() Tunnel { return first },
		Alive:   func() bool { t.Error("Alive called after Stop"); return true },
		Backoff: fastBackoff,
	}.Tunnel()

	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tun.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !first.wasStopped() {
		t.Error("the inner tunnel was not stopped")
	}
	select {
	case <-tun.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed after Stop")
	}
}

type slowStart struct {
	*fakeTunnel
	started chan struct{}
	release chan struct{}
}

func (s *slowStart) Start(context.Context) error {
	close(s.started)
	<-s.release
	return nil
}

func TestSuperviseStopDuringAReconnectStopsTheNewTunnel(t *testing.T) {
	first := newFake()
	second := &slowStart{fakeTunnel: newFake(), started: make(chan struct{}), release: make(chan struct{})}
	n := 0
	tun := Supervisor{
		New: func() Tunnel {
			n++
			if n == 1 {
				return first
			}
			return second
		},
		Alive:   func() bool { return true },
		Backoff: fastBackoff,
	}.Tunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first.drop()
	<-second.started

	stopped := make(chan struct{})
	go func() {
		_ = tun.Stop()
		close(stopped)
	}()
	time.Sleep(20 * time.Millisecond)
	close(second.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	if !second.wasStopped() {
		t.Error("the tunnel that came up during Stop was left running")
	}
}

func TestSuperviseStopDoesNotWaitForAlive(t *testing.T) {
	first := newFake()
	checking := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	tun := Supervisor{
		New:     func() Tunnel { return first },
		Alive:   func() bool { close(checking); <-release; return true },
		Backoff: fastBackoff,
	}.Tunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first.drop()
	<-checking
	start := time.Now()
	_ = tun.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Stop waited %v for the bastion status call", elapsed)
	}
}

func TestSuperviseKeepsTheLastStartError(t *testing.T) {
	first := newFake()
	failing := newFake()
	failing.startErr = errors.New("4033: not authorized")
	n := 0
	tun := Supervisor{
		New: func() Tunnel {
			n++
			if n == 1 {
				return first
			}
			return failing
		},
		Alive:   func() bool { return true },
		Backoff: fastBackoff,
	}.Tunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first.drop()
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not give up")
	}
	if err := LastStartError(tun); err == nil || !strings.Contains(err.Error(), "4033") {
		t.Errorf("LastStartError = %v, want the failed reconnect", err)
	}
	if LastStartError(first) != nil {
		t.Error("a plain tunnel has no last start error")
	}
}

func TestSuperviseClearsTheLastStartErrorAfterAReconnect(t *testing.T) {
	first, failing, second := newFake(), newFake(), newFake()
	failing.startErr = errors.New("4033: not authorized")
	tunnels := []*fakeTunnel{first, failing, second}
	n := 0
	up := make(chan struct{})
	tun := Supervisor{
		New:     func() Tunnel { n++; return tunnels[n-1] },
		Alive:   func() bool { return true },
		Backoff: fastBackoff,
		OnUp:    func() { close(up) },
	}.Tunnel()
	if err := tun.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first.drop()
	select {
	case <-up:
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect")
	}
	if err := LastStartError(tun); err != nil {
		t.Errorf("LastStartError = %v after a reconnect, want nil", err)
	}
	_ = tun.Stop()
}

func TestLastLinesSkipsBlankLinesAndGcloudAdvice(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		lines []string
		want  string
	}{
		{"no output", nil, "(none)"},
		{"last three lines", []string{"a", "b", "c", "d"}, "b | c | d"},
		{"blank lines do not count", []string{"a", "b", "", "c", " "}, "a | b | c"},
		{"only advice", []string{"Or, to investigate an IAP tunneling issue:", "gcloud compute ssh x --troubleshoot"}, "(none)"},
		{"advice between the cause and the error", []string{
			"Connection closed by UNKNOWN port 65535",
			"Recommendation: To check for possible causes of SSH connectivity issues and get",
			"recommendations, rerun the ssh command with the --troubleshoot option.",
			"ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255].",
		}, "Connection closed by UNKNOWN port 65535 | ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255]."},
	} {
		if got := lastLines(tc.lines, 3); got != tc.want {
			t.Errorf("lastLines(%s) = %q, want %q", tc.desc, got, tc.want)
		}
	}
}
