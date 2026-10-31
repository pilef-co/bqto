package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/internal/bastion"
	"github.com/pilef-co/bqto/internal/cloudsql"
	"github.com/pilef-co/bqto/internal/redis"
	"github.com/pilef-co/bqto/internal/tunnel"
	"github.com/pilef-co/bqto/internal/valkey"
)

type fakeTunnel struct {
	startErr error
	done     chan struct{}
	started  bool
	stopped  bool
	onStart  func()
}

func (f *fakeTunnel) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.started = true
	if f.onStart != nil {
		go f.onStart()
	}
	return nil
}
func (f *fakeTunnel) Done() <-chan struct{} { return f.done }
func (f *fakeTunnel) Stop() error           { f.stopped = true; return nil }
func (f *fakeTunnel) Stderr() []string      { return nil }

type deleteCall struct {
	zone, name string
	mayExist   bool
}

type fakeCloud struct {
	mu          sync.Mutex
	createErr   error
	status      string
	starts      []error
	tunnels     []*fakeTunnel
	forwards    [][]tunnel.Forward
	deletes     []deleteCall
	reserveErr  error
	reserveFrom int
	listenErr   error
	ports       fakePorts
	listened    []int
	inner       int
	reserved    map[int]bool
	reservedLog []int
	deleteErr   error
	forgotten   []uint64
	onOpen      func(n int)
	clock       time.Time
	afterOpen   func(*fakeTunnel)
}

func (f *fakeCloud) deps() sessionDeps {
	return sessionDeps{
		create: func(ctx context.Context, _ bastion.Spec, zones []string, _ func(string, string)) (string, error) {
			if f.createErr != nil {
				return "", f.createErr
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return zones[0], nil
		},
		delete: func(_ context.Context, _, zone, name string, mayExist bool) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deletes = append(f.deletes, deleteCall{zone, name, mayExist})
			return f.deleteErr == nil, f.deleteErr
		},
		status: func(context.Context, string, string, string) (string, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status, f.status != "", nil
		},
		hasSudo: func(context.Context, string, string, string) (bool, error) { return false, nil },
		vmID:    func(context.Context, string, string, string) (uint64, error) { return 4242, nil },
		forget: func(_, _ string, id uint64) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forgotten = append(f.forgotten, id)
			return nil
		},
		newTunnel: func(_, _, _ string, fwds []tunnel.Forward) tunnel.Tunnel {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forwards = append(f.forwards, fwds)
			t := &fakeTunnel{done: make(chan struct{})}
			if f.onOpen != nil {
				f.onOpen(len(f.tunnels) + 1)
			}
			if len(f.starts) > 0 {
				t.startErr, f.starts = f.starts[0], f.starts[1:]
			}
			f.tunnels = append(f.tunnels, t)
			if f.afterOpen != nil {
				t.onStart = func() { f.afterOpen(t) }
			}
			return t
		},
		listen: func(port int) (net.Listener, error) {
			if f.listenErr != nil {
				return nil, f.listenErr
			}
			ln, err := f.ports.listen(port)
			if err == nil {
				f.mu.Lock()
				f.listened = append(f.listened, port)
				f.mu.Unlock()
			}
			return ln, err
		},
		innerPort: func() (int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.inner++
			return 40000 + f.inner, nil
		},
		reserve: func(want int) (int, func(), error) {
			if f.reserveErr != nil && want >= f.reserveFrom {
				return 0, nil, f.reserveErr
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.reserved == nil {
				f.reserved = map[int]bool{}
			}
			p := want
			for f.reserved[p] {
				p++
			}
			f.reserved[p] = true
			f.reservedLog = append(f.reservedLog, p)
			return p, func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				delete(f.reserved, p)
			}, nil
		},
		now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.clock
		},
	}
}

func (f *fakeCloud) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func testSession(cmd ...string) session {
	cfg := connectConfig{Project: "p", Service: "cloudsql", Instance: "sql", Duration: "30m", Cmd: cmd}
	return session{
		cfg:       cfg,
		cfgs:      []connectConfig{cfg},
		targets:   []target{goldenTarget("cloudsql", cloudsql.Target(cloudsql.Endpoint{IP: "10.0.0.3", Port: 5432}, "sql", "", ""))},
		spec:      bastion.Spec{Project: "p", Name: "bqto-12345678"},
		zones:     []string{"us-central1-a"},
		imageName: "debian-13",
		dur:       30 * time.Minute,
	}
}

func TestRunSessionAlwaysDeletesTheBastion(t *testing.T) {
	t.Setenv("BQTO_PORT", "0")
	t.Setenv("PGPORT", "0")
	cancelled := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		return ctx, cancel
	}

	tests := []struct {
		desc       string
		cloud      *fakeCloud
		cmd        []string
		expire     bool
		session    func(*session)
		wantErr    string
		wantCode   int
		wantDelete deleteCall
		wantOpens  int
		wantPorts  []int
	}{
		{
			desc:       "the API refuses the bastion",
			cloud:      &fakeCloud{createErr: &googleapi.Error{Code: http.StatusForbidden, Message: "denied"}},
			wantErr:    "create bastion",
			wantDelete: deleteCall{zone: "", name: "bqto-12345678", mayExist: false},
		},
		{
			desc:       "a wrapped API error is not prefixed twice",
			cloud:      &fakeCloud{createErr: gcperr.Wrap("create bastion bqto-12345678 in us-central1-a (insert VM)", &googleapi.Error{Code: http.StatusForbidden, Message: "denied"})},
			wantErr:    "create bastion bqto-12345678 in us-central1-a (insert VM): denied\nFix:",
			wantDelete: deleteCall{zone: "", name: "bqto-12345678", mayExist: false},
		},
		{
			desc:       "the create call is cut off, so the bastion may exist",
			cloud:      &fakeCloud{createErr: errors.New("connection reset")},
			wantErr:    "create bastion",
			wantDelete: deleteCall{zone: "", name: "bqto-12345678", mayExist: true},
		},
		{
			desc:    "no free local port, found before the bastion",
			cloud:   &fakeCloud{listenErr: errors.New("no port")},
			wantErr: "find free port",
		},
		{
			desc:    "an asked-for port is taken, found before the bastion",
			cloud:   &fakeCloud{listenErr: errors.New("no port")},
			session: func(s *session) { s.cfgs[0].Port = 15432 },
			wantErr: "local port 15432 is taken",
		},
		{
			desc:       "the default port is held from the start, so another session moves past it",
			cloud:      &fakeCloud{ports: fakePorts{taken: map[int]bool{15432: true}}},
			cmd:        []string{"sh", "-c", `test "$BQTO_PORT" = 15433 && test "$PGPORT" = 15433`},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
			wantPorts:  []int{15433},
		},
		{
			desc:       "-- CMD keeps its port when the tunnel's inner port is taken",
			cloud:      &fakeCloud{starts: []error{tunnel.ErrPortTaken}},
			cmd:        []string{"sh", "-c", `test "$BQTO_PORT" = 15432 && test "$PGPORT" = 15432`},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  2,
			wantPorts:  []int{15432},
		},
		{
			desc:       "-- CMD with -p keeps its port when the tunnel's inner port is taken",
			cloud:      &fakeCloud{starts: []error{tunnel.ErrPortTaken}},
			cmd:        []string{"sh", "-c", `test "$BQTO_PORT" = 15432`},
			session:    func(s *session) { s.cfgs[0].Port = 15432 },
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  2,
			wantPorts:  []int{15432},
		},
		{
			desc:       "the tunnel does not open",
			cloud:      &fakeCloud{starts: []error{errors.New("4033: not authorized")}},
			wantErr:    "start tunnel",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "another session keeps taking the port",
			cloud:      &fakeCloud{starts: []error{tunnel.ErrPortTaken, tunnel.ErrPortTaken, tunnel.ErrPortTaken, tunnel.ErrPortTaken}},
			wantErr:    "start tunnel",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  portRetries + 1,
		},
		{
			desc:       "Ctrl-C after the port was taken twice",
			cloud:      &fakeCloud{starts: []error{tunnel.ErrPortTaken, tunnel.ErrPortTaken}},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  3,
		},
		{
			desc:       "Ctrl-C while the bastion is created",
			cloud:      &fakeCloud{},
			wantErr:    "create bastion",
			wantDelete: deleteCall{zone: "", name: "bqto-12345678", mayExist: true},
		},
		{
			desc:       "Ctrl-C while another session holds the port",
			cloud:      &fakeCloud{starts: []error{tunnel.ErrPortTaken}},
			wantErr:    "start tunnel",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  2,
		},
		{
			desc:  "a service cannot reserve its extra port, with the tunnel already open",
			cloud: &fakeCloud{reserveErr: errors.New("no port"), reserveFrom: 19092},
			session: func(s *session) {
				s.cfgs = append(s.cfgs, connectConfig{Project: "p", Service: fakeSOCKS, Instance: "us-central1/thing"})
				s.targets = append(s.targets, socksTarget("us-central1/thing"))
			},
			wantErr:    "find thing port",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:  "a service's own start fails, with the tunnel already open",
			cloud: &fakeCloud{},
			session: func(s *session) {
				s.cfg.Service = fakeSOCKS
				s.targets[0] = socksTarget("us-central1/thing")
				s.targets[0].spec.Start = func(context.Context, ext.Ready) (ext.Started, error) {
					return ext.Started{}, errors.New("thing would not start")
				}
			},
			wantErr:    "thing would not start",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "the session reaches its deadline",
			cloud:      &fakeCloud{},
			expire:     true,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "the bastion disappears before its deadline",
			cloud:      &fakeCloud{afterOpen: func(t *fakeTunnel) { close(t.done) }},
			wantErr:    "session ended: the bastion was stopped or deleted before its deadline",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:   "an inner port taken while the bastion starts keeps both session ports",
			cloud:  &fakeCloud{starts: []error{tunnel.ErrPortTaken}},
			expire: true,
			session: func(s *session) {
				s.cfgs[0].Port = 15432
				s.cfgs = append(s.cfgs, connectConfig{Project: "p", Service: "redis", Instance: "us-central1/cache"})
				s.targets = append(s.targets, goldenTarget("redis", redis.Target(redis.Endpoint{IP: "10.0.0.4", Port: 6379}, "us-central1/cache", "", "")))
			},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  2,
			wantPorts:  []int{15432, 16379},
		},
		{
			desc:       "-- CMD gets its port in BQTO_PORT and PGPORT",
			cloud:      &fakeCloud{},
			cmd:        []string{"sh", "-c", `test "$BQTO_PORT" = 15432 && test "$PGPORT" = 15432 && test "$BQTO_REMOTE" = 10.0.0.3:5432 && test -z "$BQTO_TLS" && test -z "$BQTO_AUTH"`},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:  "-- CMD gets the service's remote address, credential and its own variables",
			cloud: &fakeCloud{},
			cmd:   []string{"sh", "-c", `test "$BQTO_REMOTE" = thing.example:9092 && test "$BQTO_THING_PORT" = 19092 && test -z "$BQTO_TLS" && test "$BQTO_AUTH" = sasl && test "$ALL_PROXY" = socks5h://127.0.0.1:1080`},
			session: func(s *session) {
				s.cfg.Service, s.cfgs[0].Service = fakeSOCKS, fakeSOCKS
				s.targets[0] = socksTarget("us-central1/thing")
			},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:  "-- CMD through a SOCKS proxy gets the service's own address in BQTO_REMOTE",
			cloud: &fakeCloud{},
			cmd:   []string{"sh", "-c", `test "$BQTO_REMOTE" = 10.0.0.7:6379`},
			session: func(s *session) {
				s.cfg.Service, s.cfgs[0].Service = fakeSOCKS, fakeSOCKS
				s.targets[0] = socksTarget("us-central1/cache")
				s.targets[0].spec.Remote, s.targets[0].spec.Start = "10.0.0.7:6379", nil
			},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:  "-- CMD with TLS and IAM auth gets BQTO_TLS=1 and BQTO_AUTH=iam",
			cloud: &fakeCloud{},
			cmd:   []string{"sh", "-c", `test "$BQTO_TLS" = 1 && test "$BQTO_AUTH" = iam`},
			session: func(s *session) {
				s.cfg.Service, s.cfgs[0].Service = "valkey", "valkey"
				s.targets[0] = goldenTarget("valkey", valkey.Target(valkey.Endpoint{IP: "10.0.0.7", Port: 6379, TLS: true, IAMAuth: true}, "us-central1/kv", "", ""))
			},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:  "-- CMD on a Redis instance with an AUTH string gets BQTO_AUTH=password",
			cloud: &fakeCloud{},
			cmd:   []string{"sh", "-c", `test -z "$BQTO_TLS" && test "$BQTO_AUTH" = password`},
			session: func(s *session) {
				s.cfg.Service, s.cfgs[0].Service = "redis", "redis"
				s.targets[0] = goldenTarget("redis", redis.Target(redis.Endpoint{IP: "10.0.0.7", Port: 6379, Auth: true}, "us-central1/cache", "", ""))
			},
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD with a path that is gone by the time it starts exits 127",
			cloud:      &fakeCloud{},
			cmd:        []string{"./bqto-no-such-script.sh"},
			wantCode:   exitCmdNotFound,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "a failed delete after a normal end exits 1",
			cloud:      &fakeCloud{deleteErr: errors.New("delete vm: boom")},
			expire:     true,
			wantCode:   exitError,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD exits 0 and the delete fails: exit 1",
			cloud:      &fakeCloud{deleteErr: errors.New("delete vm: boom")},
			cmd:        []string{"true"},
			wantCode:   exitError,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD exits 3 and the delete fails: the command's 3 is kept",
			cloud:      &fakeCloud{deleteErr: errors.New("delete vm: boom")},
			cmd:        []string{"sh", "-c", "exit 3"},
			wantCode:   3,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "a failed delete after Ctrl-C during the create exits 1",
			cloud:      &fakeCloud{deleteErr: errors.New("delete vm: boom")},
			wantCode:   exitError,
			wantDelete: deleteCall{zone: "", name: "bqto-12345678", mayExist: true},
		},
		{
			desc:       "a failed delete keeps the error that ended the session",
			cloud:      &fakeCloud{deleteErr: errors.New("delete vm: boom"), starts: []error{errors.New("4033: not authorized")}},
			wantErr:    "start tunnel",
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD that is gone by the time it starts exits 127",
			cloud:      &fakeCloud{},
			cmd:        []string{"bqto-no-such-command"},
			wantCode:   exitCmdNotFound,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD killed by SIGTERM exits 143, 128 + the signal",
			cloud:      &fakeCloud{},
			cmd:        []string{"sh", "-c", "kill -TERM $$"},
			wantCode:   143,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD killed by SIGKILL exits 137",
			cloud:      &fakeCloud{},
			cmd:        []string{"sh", "-c", "kill -KILL $$"},
			wantCode:   137,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
		{
			desc:       "-- CMD passes on its exit code",
			cloud:      &fakeCloud{},
			cmd:        []string{"sh", "-c", "exit 3"},
			wantCode:   3,
			wantDelete: deleteCall{zone: "us-central1-a", name: "bqto-12345678"},
			wantOpens:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			tc.cloud.clock = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			if tc.expire {
				tc.cloud.afterOpen = func(tun *fakeTunnel) {
					tc.cloud.advance(time.Hour)
					close(tun.done)
				}
			}
			ctx, cancel := cancelled()
			defer cancel()
			switch tc.desc {
			case "Ctrl-C after the port was taken twice":
				tc.cloud.afterOpen = func(*fakeTunnel) { cancel() }
			case "Ctrl-C while the bastion is created", "a failed delete after Ctrl-C during the create exits 1":
				cancel()
			case "Ctrl-C while another session holds the port":
				tc.cloud.onOpen = func(n int) {
					if n == 2 {
						cancel()
					}
				}
			}

			s := testSession(tc.cmd...)
			if tc.session != nil {
				tc.session(&s)
			}
			err := runSession(ctx, s, tc.cloud.deps())

			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
				}
			case tc.wantCode != 0:
				err = interrupted(err, ctx.Err())
				var ec exitCodeError
				if !errors.As(err, &ec) || ec.code != tc.wantCode {
					t.Errorf("err = %v, want exit code %d", err, tc.wantCode)
				}
			case err != nil:
				t.Errorf("err = %v, want nil", err)
			}

			if tc.wantDelete == (deleteCall{}) {
				if len(tc.cloud.deletes) != 0 {
					t.Errorf("deletes = %+v, want none: no bastion was created", tc.cloud.deletes)
				}
			} else if len(tc.cloud.deletes) != 1 || tc.cloud.deletes[0] != tc.wantDelete {
				t.Errorf("deletes = %+v, want exactly %+v", tc.cloud.deletes, tc.wantDelete)
			}
			created := tc.wantDelete.zone != ""
			if wantForget := created && tc.cloud.deleteErr == nil; wantForget != (len(tc.cloud.forgotten) == 1 && tc.cloud.forgotten[0] == 4242) {
				t.Errorf("forgotten = %v, want the bastion's host key and certificate removed: %v", tc.cloud.forgotten, wantForget)
			}
			if len(tc.cloud.tunnels) != tc.wantOpens {
				t.Errorf("opened %d tunnels, want %d", len(tc.cloud.tunnels), tc.wantOpens)
			}
			if tc.wantPorts != nil && !slices.Equal(tc.cloud.listened, tc.wantPorts) {
				t.Errorf("session held ports %v, want %v", tc.cloud.listened, tc.wantPorts)
			}
			if open := tc.cloud.ports.open(); len(open) != 0 {
				t.Errorf("ports %v still held after the session ended", open)
			}
			seen := map[int]bool{}
			for _, fwds := range tc.cloud.forwards {
				for _, f := range fwds {
					if slices.Contains(tc.cloud.listened, f.LocalPort) || seen[f.LocalPort] {
						t.Errorf("tunnel forwards from port %d, want a fresh inner port, never a session port", f.LocalPort)
					}
					seen[f.LocalPort] = true
				}
			}
			for i, tun := range tc.cloud.tunnels {
				if tun.started && !tun.stopped {
					t.Errorf("tunnel %d started and was never stopped", i+1)
				}
			}

		})
	}
}

func answering(t *testing.T, reply string) int {
	t.Helper()
	ln, err := tunnel.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(reply))
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func readPort(port int) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 16)
	n, _ := c.Read(b)
	return string(b[:n])
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPortRouterRoutesOnlyAnUpTunnel(t *testing.T) {
	held, err := tunnel.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	port := held.Addr().(*net.TCPAddr).Port
	hold := tunnel.NewHold(held)
	defer hold.Close()
	router := &portRouter{holds: []*tunnel.Hold{hold}}
	first, second := answering(t, "first\n"), answering(t, "second\n")

	if got := readPort(port); got != "" {
		t.Errorf("before any tunnel is up, a connection read %q, want it closed", got)
	}
	router.up(1, []int{first})
	if got := readPort(port); got != "first\n" {
		t.Errorf("with tunnel 1 up, read %q", got)
	}
	router.down(1)
	if got := readPort(port); got != "" {
		t.Errorf("with tunnel 1 down, a connection read %q, want it closed", got)
	}
	router.up(2, []int{second})
	router.down(1)
	if got := readPort(port); got != "second\n" {
		t.Errorf("the old tunnel's drop unrouted the new one: read %q", got)
	}
}

func TestARoutedTunnelUnroutesWhenItDrops(t *testing.T) {
	held, err := tunnel.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	port := held.Addr().(*net.TCPAddr).Port
	hold := tunnel.NewHold(held)
	defer hold.Close()
	router := &portRouter{holds: []*tunnel.Hold{hold}}
	backend := answering(t, "up\n")
	fake := &fakeTunnel{done: make(chan struct{})}
	tun := router.open([]target{{}}, func() (int, error) { return backend, nil }, func([]tunnel.Forward) tunnel.Tunnel { return fake })
	if err := tun.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readPort(port); got != "up\n" {
		t.Fatalf("with the tunnel up, read %q", got)
	}
	close(fake.done)
	eventually(t, "the dropped tunnel is unrouted", func() bool { return readPort(port) == "" })
}

func TestInnerPortTakenIsTransientWithNoPortAdvice(t *testing.T) {
	router := &portRouter{}
	tun := router.open([]target{{}}, func() (int, error) { return 40001, nil }, func([]tunnel.Forward) tunnel.Tunnel {
		return &fakeTunnel{startErr: tunnel.ErrPortTaken, done: make(chan struct{})}
	})
	err := tun.Start(context.Background())
	if !errors.Is(err, errInnerPortTaken) || errors.Is(err, tunnel.ErrPermanent) {
		t.Fatalf("err = %v, want errInnerPortTaken, not permanent, so the supervisor tries again", err)
	}
	if strings.Contains(err.Error(), "-p") {
		t.Errorf("err = %q, must not tell the user to pick another -p: bqto holds the session port", err)
	}
}

func TestADroppedTunnelKeepsTheSessionPort(t *testing.T) {
	t.Setenv("BQTO_PORT", "0")
	t.Setenv("PGPORT", "0")
	cloud := &fakeCloud{status: "RUNNING", clock: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	var heldDuringDrop error
	cloud.onOpen = func(n int) {
		if n != 2 {
			return
		}
		if ln, err := cloud.ports.listen(15432); err == nil {
			ln.Close()
			heldDuringDrop = errors.New("port 15432 could be bound while the tunnel reconnected")
		}
	}
	cloud.afterOpen = func(tun *fakeTunnel) {
		cloud.mu.Lock()
		n := len(cloud.tunnels)
		if n == 2 {
			cloud.status = ""
		}
		cloud.mu.Unlock()
		if n == 2 {
			cloud.advance(time.Hour)
		}
		close(tun.done)
	}
	if err := runSession(context.Background(), testSession(), cloud.deps()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(cloud.tunnels) != 2 {
		t.Fatalf("opened %d tunnels, want the reconnect", len(cloud.tunnels))
	}
	if heldDuringDrop != nil {
		t.Error(heldDuringDrop)
	}
	if cloud.forwards[0][0].LocalPort == cloud.forwards[1][0].LocalPort {
		t.Errorf("forwards = %v, want the reconnect on a fresh inner port", cloud.forwards)
	}
	if open := cloud.ports.open(); len(open) != 0 {
		t.Errorf("ports %v still held after the session ended", open)
	}
}

func TestTwoServicesReserveTwoExtraPorts(t *testing.T) {
	cloud := &fakeCloud{reserved: map[int]bool{19093: true}, clock: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	cloud.afterOpen = func(tun *fakeTunnel) {
		cloud.advance(time.Hour)
		close(tun.done)
	}
	s := testSession()
	s.cfg.Service = fakeSOCKS
	s.cfgs = []connectConfig{{Project: "p", Service: fakeSOCKS, Instance: "us-central1/a"}, {Project: "p", Service: fakeSOCKS, Instance: "us-central1/b"}}
	s.targets = []target{socksTarget("us-central1/a"), socksTarget("us-central1/b")}
	if err := runSession(context.Background(), s, cloud.deps()); err != nil {
		t.Fatal(err)
	}
	if want := []int{19092, 19094}; !slices.Equal(cloud.reservedLog, want) {
		t.Errorf("extra ports = %v, want %v: 19093 is another session's", cloud.reservedLog, want)
	}
	if len(cloud.reserved) != 1 || !cloud.reserved[19093] {
		t.Errorf("reserved after the session = %v, want only the other session's 19093", cloud.reserved)
	}
}
