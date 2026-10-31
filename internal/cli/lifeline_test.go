package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	return report(io.Discard, err)
}

func TestStdinLifelineOnlyWatchesWithTheFlag(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		flag     bool
		wantDone bool
	}{
		{desc: "connect </dev/null without the flag keeps the session", flag: false, wantDone: false},
		{desc: "connect </dev/null with the flag ends the session", flag: true, wantDone: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			devNull, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer devNull.Close()
			cfgs := []connectConfig{{Service: "cloudsql", StdinClose: tc.flag}}
			ctx, err := stdinLifeline(context.Background(), cfgs, devNull, false)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				if !tc.wantDone {
					t.Error("the session ended although the flag is off")
				}
			case <-time.After(300 * time.Millisecond):
				if tc.wantDone {
					t.Error("the session did not end on EOF")
				}
			}
		})
	}
}

func TestStdinLifelineIgnoresDataUntilEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, err := stdinLifeline(context.Background(), []connectConfig{{StdinClose: true}}, r, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("keepalive\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("data on stdin ended the session")
	case <-time.After(100 * time.Millisecond):
	}
	_ = w.Close()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing stdin did not end the session")
	}
}

func TestClosingStdinEndsTheSessionAsSIGTERMDoes(t *testing.T) {
	t.Setenv("BQTO_PORT", "0")
	t.Setenv("PGPORT", "0")
	ends := []struct {
		desc string
		end  func(w *os.File) error
	}{
		{"SIGTERM", func(*os.File) error { return syscall.Kill(os.Getpid(), syscall.SIGTERM) }},
		{"the parent dies and stdin closes", func(w *os.File) error { return w.Close() }},
	}
	type outcome struct {
		code    int
		deletes []deleteCall
	}
	var got []outcome
	for _, e := range ends {
		t.Run(e.desc, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			sigCtx, stop := signal.NotifyContext(context.Background(), sessionSignals...)
			defer stop()
			s := testSession()
			s.cfg.StdinClose, s.cfgs[0].StdinClose = true, true
			ctx, err := stdinLifeline(sigCtx, s.cfgs, r, false)
			if err != nil {
				t.Fatal(err)
			}
			opened := make(chan struct{})
			var once sync.Once
			cloud := &fakeCloud{afterOpen: func(*fakeTunnel) { once.Do(func() { close(opened) }) }}
			result := make(chan error, 1)
			go func() { result <- runSession(ctx, s, cloud.deps()) }()
			select {
			case <-opened:
			case <-time.After(5 * time.Second):
				t.Fatal("the tunnel never opened")
			}
			if err := e.end(w); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("the session did not end")
			}
			want := []deleteCall{{zone: "us-central1-a", name: "bqto-12345678"}}
			cloud.mu.Lock()
			deletes := slices.Clone(cloud.deletes)
			cloud.mu.Unlock()
			if !slices.Equal(deletes, want) {
				t.Errorf("deletes = %v, want %v", deletes, want)
			}
			got = append(got, outcome{code: exitCodeOf(interrupted(err, ctx.Err())), deletes: deletes})
		})
	}
	if len(got) == 2 && got[0].code != 0 {
		t.Errorf("SIGTERM once the session is ready gave exit code %d, want 0", got[0].code)
	}
	if len(got) == 2 && (got[0].code != got[1].code || !slices.Equal(got[0].deletes, got[1].deletes)) {
		t.Errorf("SIGTERM gave %+v and a closed stdin gave %+v, want the same", got[0], got[1])
	}
}
