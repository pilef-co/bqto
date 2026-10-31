package tunnel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pilef-co/bqto/ext"
)

const (
	reconnectTimeout = 2 * time.Minute
	maxReconnects    = 5
	baseWait         = 2 * time.Second
	maxWait          = 30 * time.Second
)

type Supervisor struct {
	New     func() Tunnel
	Alive   func() bool
	OnRetry func(attempt int, wait time.Duration)
	OnUp    func()
	Backoff func(attempt int) time.Duration
}

func (s Supervisor) Tunnel() Tunnel {
	if s.Backoff == nil {
		s.Backoff = retryWait
	}
	return &supervised{
		sup:   s,
		doneC: make(chan struct{}),
		stopC: make(chan struct{}),
	}
}

func retryWait(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	w := baseWait
	for i := 1; i < attempt; i++ {
		w *= 2
		if w >= maxWait {
			return maxWait
		}
	}
	return w
}

type supervised struct {
	sup   Supervisor
	doneC chan struct{}
	stopC chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	cur      Tunnel
	watching bool
	lastErr  error
}

func (t *supervised) lastStartError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

func LastStartError(tun Tunnel) error {
	if s, ok := tun.(*supervised); ok {
		return s.lastStartError()
	}
	return nil
}

func (t *supervised) Start(ctx context.Context) error {
	first := t.sup.New()
	if err := first.Start(ctx); err != nil {
		t.setCurrent(first)
		return err
	}
	t.mu.Lock()
	t.cur = first
	t.watching = true
	t.mu.Unlock()
	go t.watch(first)
	return nil
}

func (t *supervised) Done() <-chan struct{} { return t.doneC }

func (t *supervised) Stderr() []string {
	t.mu.Lock()
	cur := t.cur
	t.mu.Unlock()
	if cur == nil {
		return nil
	}
	return cur.Stderr()
}

func (t *supervised) Stop() error {
	t.stopOnce.Do(func() { close(t.stopC) })
	t.mu.Lock()
	watching := t.watching
	t.mu.Unlock()
	if watching {
		<-t.doneC
	}
	t.mu.Lock()
	cur := t.cur
	t.mu.Unlock()
	if cur != nil {
		return cur.Stop()
	}
	return nil
}

func (t *supervised) setCurrent(next Tunnel) {
	t.mu.Lock()
	t.cur = next
	t.mu.Unlock()
}

func (t *supervised) watch(cur Tunnel) {
	defer close(t.doneC)

	attempt := 0
	for {
		select {
		case <-t.stopC:
			return
		case <-cur.Done():
		}
		ext.Debugf("tunnel: gcloud exited; its last output: %s", lastLines(cur.Stderr(), 3))

		if closed(t.stopC) || !t.alive() {
			return
		}

		attempt++
		if attempt > maxReconnects {
			return
		}
		wait := t.sup.Backoff(attempt)
		if t.sup.OnRetry != nil {
			t.sup.OnRetry(attempt, wait)
		}
		select {
		case <-t.stopC:
			return
		case <-time.After(wait):
		}

		next := t.sup.New()
		if err := t.start(next); err != nil {
			ext.Debugf("tunnel: reconnect %d failed: %v", attempt, err)
			t.mu.Lock()
			t.lastErr = err
			t.mu.Unlock()
			t.setCurrent(next)
			if errors.Is(err, ErrPermanent) {
				return
			}
			continue
		}
		t.mu.Lock()
		t.lastErr = nil
		t.mu.Unlock()
		t.setCurrent(next)
		cur = next
		attempt = 0
		if t.sup.OnUp != nil {
			t.sup.OnUp()
		}
	}
}

func lastLines(lines []string, n int) string {
	lines = withoutAdvice(lines)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, " | ")
}

func withoutAdvice(lines []string) []string {
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" || isGcloudAdvice(l) {
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

func isGcloudAdvice(line string) bool {
	return strings.Contains(line, "--troubleshoot") ||
		strings.HasPrefix(line, "Recommendation: To check for possible causes") ||
		strings.HasPrefix(line, "Or, to investigate an IAP tunneling issue")
}

func (t *supervised) alive() bool {
	result := make(chan bool, 1)
	go func() { result <- t.sup.Alive() }()
	select {
	case <-t.stopC:
		return false
	case ok := <-result:
		return ok
	}
}

func (t *supervised) start(next Tunnel) error {
	ctx, cancel := context.WithTimeout(context.Background(), reconnectTimeout)
	defer cancel()
	go func() {
		select {
		case <-t.stopC:
			cancel()
		case <-ctx.Done():
		}
	}()
	return next.Start(ctx)
}
