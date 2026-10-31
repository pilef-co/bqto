package tunnel

import (
	"github.com/pilef-co/bqto/ext"

	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Forward struct {
	LocalPort  int
	RemoteHost string
	RemotePort int
}

type Tunnel interface {
	Start(ctx context.Context) error

	Done() <-chan struct{}

	Stop() error
	Stderr() []string
}

func Gcloud(project, zone, name string, fwds []Forward) Tunnel {
	return &gcloudTunnel{
		argv:  buildArgv(project, zone, name, fwds),
		fwds:  fwds,
		doneC: make(chan struct{}),
	}
}

func buildArgv(project, zone, name string, fwds []Forward) []string {
	args := []string{
		"gcloud", "compute", "ssh", name,
		"--project", project,
		"--zone", zone,
		"--tunnel-through-iap",
		"--quiet",
		"--",
		"-N",
		"-v",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
	}
	for _, f := range fwds {
		if f.RemoteHost == "" {

			args = append(args, "-D", fmt.Sprintf("127.0.0.1:%d", f.LocalPort))
		} else {

			args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:%s:%d", f.LocalPort, f.RemoteHost, f.RemotePort))
		}
	}
	return args
}

func FreePort(want int) (int, error) {
	for p := want; p < want+PortSearchRange; p++ {
		if ln, err := Listen(p); err == nil {
			ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free local port from %d to %d\nFix: %s", want, want+PortSearchRange-1, PortRangeTakenFix())
}

func isPermanentTunnelError(lines []string) bool {
	for _, l := range lines {
		if iapRefused.MatchString(l) || strings.Contains(l, "Permission denied (publickey") || strings.Contains(l, certificatesNotGA) {
			return true
		}
	}
	return bindFailed(lines)
}

func bootRace(lines []string) bool {
	denied := false
	for _, l := range lines {
		if iapRefused.MatchString(l) || strings.Contains(l, certificatesNotGA) {
			return false
		}
		if strings.Contains(l, "Permission denied (publickey") {
			denied = true
		}
	}
	return denied
}

func bindFailed(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "Address already in use") || strings.Contains(l, "Cannot bind port") {
			return true
		}
	}
	return false
}

const (
	stopGrace    = 3 * time.Second
	pollInterval = time.Second
)

var (
	retryDelay     = 3 * time.Second
	publickeyGrace = time.Minute
)

type gcloudTunnel struct {
	argv  []string
	fwds  []Forward
	doneC chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	live     *stderrRing
}

func (t *gcloudTunnel) Done() <-chan struct{} {
	return t.doneC
}

func (t *gcloudTunnel) Stderr() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live == nil {
		return nil
	}
	return WithoutDebug(t.live.lines())
}

func (t *gcloudTunnel) Start(ctx context.Context) error {
	var lastStderr []string
	var firstDenial time.Time
	var retryAfter string

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return notReady(err, lastStderr)
		}

		procDone := make(chan struct{})
		buf := &stderrRing{max: 60}

		if attempt > 1 {
			ext.Debugf("tunnel: attempt %d, after: %s", attempt, retryAfter)
		}
		ext.Debugf("gcloud: %s", strings.Join(t.argv, " "))
		cmd := exec.Command(t.argv[0], t.argv[1:]...) //nolint:gosec // argv is the gcloud compute ssh command bqto builds
		cmd.Stderr = buf
		setGroup(cmd)

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start gcloud: %w", err)
		}

		t.mu.Lock()
		t.cmd = cmd
		t.exited = procDone
		t.live = buf
		t.mu.Unlock()

		go func() {
			_ = cmd.Wait()
			close(procDone)
		}()

		if t.pollPorts(ctx, procDone, buf) {

			go func() {
				<-procDone
				close(t.doneC)
			}()
			return nil
		}

		lastStderr = WithoutDebug(buf.lines())

		if bindFailed(lastStderr) {
			return permanent{portTakenError(lastStderr)}
		}
		if bootRace(lastStderr) && firstDenial.IsZero() {
			firstDenial = time.Now()
		}
		if isPermanentTunnelError(lastStderr) && (!bootRace(lastStderr) || time.Since(firstDenial) >= publickeyGrace) {
			return permanent{describe(errors.New("gcloud failed"), lastStderr, "run bqto again; docs/troubleshooting.md lists the usual causes")}
		}

		if err := ctx.Err(); err != nil {

			if !closed(procDone) {
				_ = terminateTree(cmd)
				time.AfterFunc(stopGrace, func() {
					if !closed(procDone) {
						_ = killTree(cmd)
					}
				})
			}
			return notReady(err, lastStderr)
		}
		retryAfter = retryReason(lastStderr, cmd.ProcessState)

		select {
		case <-ctx.Done():
			return notReady(ctx.Err(), lastStderr)
		case <-time.After(retryDelay):
		}
	}
}

func retryReason(lines []string, exit *os.ProcessState) string {
	if len(withoutAdvice(lines)) == 0 {
		return "gcloud " + exit.String() + " with no output"
	}
	return lastLines(lines, 3)
}

var ErrPermanent = errors.New("permanent")

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

func (p permanent) Is(target error) bool { return target == ErrPermanent }

var boundPort = regexp.MustCompile(`bind \[?[0-9.:a-f]*\]?:(\d+): Address already in use|port (\d+): Address already in use|Cannot bind port (\d+)`)

func portTakenError(lines []string) error {
	for _, l := range lines {
		if m := boundPort.FindStringSubmatch(l); m != nil {
			port := m[1] + m[2] + m[3]
			return fmt.Errorf("%w: port %s\nFix: %s", ErrPortTaken, port, innerPortTakenFix)
		}
	}
	return fmt.Errorf("%w\nFix: %s", ErrPortTaken, innerPortTakenFix)
}

var ErrPortTaken = errors.New("another program took the tunnel's loopback port while the tunnel started")

const innerPortTakenFix = "run bqto connect again"

const PortSearchRange = 100

var proFlag = ext.FlagRegistered

func otherPort() string {
	if proFlag("stack") {
		return "pick another port with -p (in a stack: the profile's local_port, bqto profile create NAME ... -p PORT)"
	}
	return "pick another port with -p"
}

func PortTakenFix() string { return "free it, or " + otherPort() }

func PortRangeTakenFix() string { return "free one of them, or " + otherPort() }

const troubleshootingFix = "read the gcloud lines above; docs/troubleshooting.md lists the usual causes"

func notReady(ctxErr error, lines []string) error {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return describe(errors.New("the tunnel was not ready in time"), lines, "run bqto again; docs/troubleshooting.md lists the usual causes")
	}
	return describe(errors.New("cancelled before the tunnel was ready"), lines, "")
}

func describe(err error, lines []string, fixWithoutLines string) error {
	if cause, ok := Explain(lines); ok {
		return fmt.Errorf("%w: %s", err, cause)
	}
	lines = CauseLines(lines)
	switch {
	case len(lines) > 0 && fixWithoutLines != "":
		return fmt.Errorf("%w\nlast stderr:\n%s\nFix: %s", err, strings.Join(lines, "\n"), troubleshootingFix)
	case len(lines) > 0:
		return fmt.Errorf("%w\nlast stderr:\n%s", err, strings.Join(lines, "\n"))
	case fixWithoutLines != "":
		return fmt.Errorf("%w\nFix: %s", err, fixWithoutLines)
	}
	return err
}

func CauseLines(lines []string) []string {
	return withoutAdvice(WithoutDebug(lines))
}

func WithoutDebug(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "debug") {
			continue
		}
		out = append(out, l)
	}
	return out
}

const certificatesNotGA = "SSH using certificates is not yet generally available"

const MinGcloud = 550

var GcloudUpdateFix = fmt.Sprintf("gcloud components update, or update the Google Cloud CLI with the package manager you installed it with; bqto needs gcloud %d.0.0 or later", MinGcloud)

var iapRefused = regexp.MustCompile(`\b4033\b`)

const (
	osLoginRefused          = "OS Login refused the SSH certificate: your account lacks roles/compute.osLogin"
	osLoginRefusedWithImage = "OS Login refused the SSH certificate: either your account lacks roles/compute.osLogin, or the bastion's image does not support OS Login certificates (it needs OpenSSH 7.4 or later)"
)

var knownFailures = []struct {
	match      *regexp.Regexp
	cause, fix string
}{
	{iapRefused, "IAP refused the tunnel (4033)", "check that you have roles/iap.tunnelResourceAccessor and that the bastion is still running"},
	{regexp.MustCompile(`\b4003\b`), "IAP could not reach port 22 on the bastion (4003)", "allow ingress on tcp:22 from 35.235.240.0/20 to the bastion"},
	{regexp.MustCompile(`\b4047\b`), "IAP could not find the bastion (4047)", "run bqto connect again; if it repeats, check that the bastion is RUNNING"},
	{literal(certificatesNotGA), "this gcloud is too old for OS Login certificates, which bqto's bastions use", GcloudUpdateFix},
	{literal("Permission denied (publickey"), osLoginRefused, "grant roles/compute.osLogin on the project"},
	{literal("Address already in use"), "another program took the tunnel's loopback port while the tunnel started", innerPortTakenFix},
	{literal("Cannot bind port"), "another program took the tunnel's loopback port while the tunnel started", innerPortTakenFix},
}

func literal(s string) *regexp.Regexp { return regexp.MustCompile(regexp.QuoteMeta(s)) }

func Explain(lines []string) (string, bool) {
	for _, f := range knownFailures {
		if slices.ContainsFunc(lines, f.match.MatchString) {
			if f.cause == osLoginRefused && proFlag("image") {
				return osLoginRefusedWithImage + "\nFix: grant roles/compute.osLogin on the project; with --image, use an image that supports OS Login certificates, such as the default Debian", true
			}
			return f.cause + "\nFix: " + f.fix, true
		}
	}
	return "", false
}

func (t *gcloudTunnel) pollPorts(ctx context.Context, procDone <-chan struct{}, buf *stderrRing) bool {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-procDone:
			return false
		case <-tick.C:
			if ctx.Err() != nil {
				return false
			}
			if !buf.boundAll(t.fwds) || !allPortsOpen(t.fwds) {
				continue
			}

			if closed(procDone) {
				return false
			}
			return true
		}
	}
}

func allPortsOpen(fwds []Forward) bool {
	for _, f := range fwds {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", f.LocalPort), 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
	}
	return true
}

func (t *gcloudTunnel) Stop() error {
	t.stopOnce.Do(func() {
		t.mu.Lock()
		cmd, exited := t.cmd, t.exited
		t.mu.Unlock()
		if exited == nil {
			exited = t.doneC
		}
		if cmd == nil || cmd.Process == nil || closed(exited) {
			return
		}
		_ = terminateTree(cmd)
		select {
		case <-exited:
		case <-time.After(stopGrace):
			_ = killTree(cmd)
		}
	})
	return nil
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type stderrRing struct {
	mu    sync.Mutex
	max   int
	buf   []string
	cur   strings.Builder
	bound map[int]bool
}

const listeningMarker = "Local forwarding listening on 127.0.0.1 port "

func (r *stderrRing) noteBound(line string) {
	_, after, ok := strings.Cut(line, listeningMarker)
	if !ok {
		return
	}
	port, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(after), "."))
	if err != nil {
		return
	}
	if r.bound == nil {
		r.bound = map[int]bool{}
	}
	r.bound[port] = true
}

func (r *stderrRing) boundAll(fwds []Forward) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range fwds {
		if !r.bound[f.LocalPort] {
			return false
		}
	}
	return true
}

func (r *stderrRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range p {
		switch b {
		case '\r':
		case '\n':
			r.noteBound(r.cur.String())
			r.buf = append(r.buf, r.cur.String())
			if len(r.buf) > r.max {
				r.buf = r.buf[len(r.buf)-r.max:]
			}
			r.cur.Reset()
		default:
			r.cur.WriteByte(b)
		}
	}
	return len(p), nil
}

func (r *stderrRing) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.buf))
	copy(out, r.buf)
	if r.cur.Len() > 0 {
		out = append(out, r.cur.String())
	}
	return out
}
