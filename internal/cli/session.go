package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/internal/auth"
	"github.com/pilef-co/bqto/internal/bastion"
	"github.com/pilef-co/bqto/internal/tunnel"
	"github.com/pilef-co/bqto/quota"
)

func timeLeft(now, deadline time.Time) string {
	left := deadline.Sub(now).Round(time.Minute)
	if left < time.Minute {
		return "in less than a minute"
	}
	s := strings.TrimSuffix(left.String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return "in " + s
}

func warnDelay(now, deadline time.Time) (time.Duration, bool) {
	d := deadline.Add(-5 * time.Minute).Sub(now)
	return d, d > 0
}

type sessionExpired struct{ duration string }

func (e sessionExpired) Error() string {
	return fmt.Sprintf("the bastion reached its max duration (%s)\nFix: run bqto connect again, with a longer --duration if you need more time", e.duration)
}

const deadlineSlack = time.Minute

const portRetries = 3

func endReason(statusErr error, found bool, status string, spot bool, now, deadline time.Time, duration string, stderr []string, lastStart error) error {
	switch {
	case statusErr != nil:
		err := fmt.Errorf("the tunnel exited, and bqto could not check the bastion: %w", statusErr)
		if _, fix := gcperr.SplitFix(statusErr); fix == "" {
			return fmt.Errorf("%w\nFix: run bqto connect again", err)
		}
		return err
	case found && status == "RUNNING":
		if cause, ok := tunnel.Explain(stderr); ok {
			return fmt.Errorf("the tunnel dropped while the bastion was still running: %s", cause)
		}
		msg := "the tunnel kept dropping and bqto stopped reconnecting, although the bastion was still running (the laptop slept, the network changed, or IAP closed the connection)"
		fix := "run bqto connect again"
		if lastStart != nil {
			last, lastFix := gcperr.SplitFix(lastStart)
			msg += "; the last attempt: " + last
			fix = cmp.Or(lastFix, fix)
		}
		if lines := tunnel.CauseLines(stderr); len(lines) > 0 && lastStart == nil {
			msg += "\nlast stderr:\n" + strings.Join(lines, "\n")
		}
		return errors.New(msg + "\nFix: " + fix)
	case !now.Before(deadline.Add(-deadlineSlack)):
		return sessionExpired{duration: duration}
	case spot:
		return errors.New("the Spot bastion was preempted\nFix: run bqto connect again, or without --spot")
	}
	return errors.New("the bastion was stopped or deleted before its deadline\nFix: run bqto connect again; the project's audit log says who stopped or deleted it (docs/usage.md, Who created a bastion)")
}

func canReconnect(statusErr error, found bool, status string, now, deadline time.Time) bool {
	if statusErr != nil || !found || status != "RUNNING" {
		return false
	}
	return now.Before(deadline.Add(-deadlineSlack))
}

func vmIDWarning(err error) string {
	msg, _ := gcperr.SplitFix(err)
	return "warn: " + msg + "; its host key and certificate stay in ~/.ssh"
}

func cleanupStartMessage(mayExist bool) string {
	if mayExist {
		return "checking whether the bastion was created…"
	}
	return "deleting bastion…"
}

func cleanupMessage(project, vm string, mayExist, requested bool, err error) string {
	switch {
	case err != nil:
		msg, _ := gcperr.SplitFix(err)
		return fmt.Sprintf("error: bastion %s may still be running: %s\nFix: gcloud compute instances list --project %s --filter=labels.bqto=session, then delete it", vm, msg, project)
	case requested:
		return fmt.Sprintf("bastion %s is being deleted (about 2 minutes)", vm)
	case mayExist:
		return "no bastion was created"
	}
	return ""
}

var sessionSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

func interrupted(err, ctxErr error) error {
	var ec exitCodeError
	if err == nil || ctxErr == nil || errors.As(err, &ec) {
		return err
	}
	return exitCodeError{code: exitInterrupted}
}

func stdinLifeline(ctx context.Context, cfgs []connectConfig, stdin io.Reader, terminal bool) (context.Context, error) {
	if !slices.ContainsFunc(cfgs, func(c connectConfig) bool { return c.StdinClose }) {
		return ctx, nil
	}
	if terminal {
		return ctx, usage(errors.New("--end-on-stdin-close needs a pipe on stdin, and stdin is a terminal\nFix: drop --end-on-stdin-close, or start bqto from a program that keeps a pipe to its stdin open"))
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		if ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "stdin closed: ending the session")
		}
		cancel()
	}()
	return ctx, nil
}

func doConnect(cfgs []connectConfig, explicit map[string]bool) (err error) {
	surviveBrokenPipes()
	ctx, stop := signal.NotifyContext(context.Background(), sessionSignals...)
	defer stop()
	defer func() { err = interrupted(err, ctx.Err()) }()

	asSet := slices.Clone(cfgs)
	dur, err := prepareConfigs(cfgs)
	if err != nil {
		return err
	}
	var warnings []string
	for _, h := range ext.Connect() {
		if h.BeforeAuth != nil {
			warnings = append(warnings, h.BeforeAuth(toExtAll(asSet), toExtAll(cfgs))...)
		}
	}
	cfg := cfgs[0]
	ctx = quota.WithProject(ctx, cfg.Project)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warn:", w)
	}
	if err := findCommand(cfg.Cmd); err != nil {
		return err
	}
	ctx, err = stdinLifeline(ctx, cfgs, os.Stdin, stdinIsTerminal())
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "checking auth…")
	versionErr := make(chan error, 1)
	go func() { versionErr <- auth.CheckGcloudVersion(ctx, tunnel.MinGcloud, tunnel.GcloudUpdateFix) }()
	email, impersonated, err := auth.Check(ctx)
	if err != nil {
		return err
	}
	if err := <-versionErr; err != nil {
		return err
	}
	iapWarning, err := auth.CheckIAPTunnel(ctx, cfg.Project, email)
	if err != nil {
		return err
	}
	if iapWarning != "" {
		fmt.Fprintln(os.Stderr, "warn:", iapWarning)
	}

	targets := make([]target, 0, len(cfgs))
	for i, c := range cfgs {
		if len(cfgs) > 1 {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s %s\n", i+1, len(cfgs), c.Service, c.Instance)
		}
		t, resolveErr := resolveTarget(ctx, c, explicit)
		if resolveErr != nil {
			return resolveErr
		}
		t.account, t.impersonated = email, impersonated
		targets = append(targets, t)
	}
	netProject, network, region, warnings, err := sessionNetwork(targets)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warn:", w)
	}
	svcSubnet := targets[0].svcSubnet

	fmt.Fprintln(os.Stderr, "checking firewall…")
	tags, fwWarnings, err := bastion.CheckFirewall(ctx, netProject, network, region)
	for _, w := range fwWarnings {
		fmt.Fprintln(os.Stderr, "warn:", w)
	}
	if err != nil {
		return err
	}

	zones := []string{cfg.Zone}
	if cfg.Zone == "" {
		fmt.Fprintln(os.Stderr, "finding zones…")
		zones, err = bastion.Zones(ctx, cfg.Project, region)
		if err != nil {
			return err
		}
	}

	needsPGA := needsPrivateGoogleAccess(targets)
	vmSubnet := cmp.Or(cfg.Subnet, svcSubnet)
	if vmSubnet == "" {
		fmt.Fprintln(os.Stderr, "finding subnet…")
		if needsPGA {
			vmSubnet, err = bastion.SubnetWithPGA(ctx, netProject, network, region)
		} else {
			vmSubnet, err = bastion.Subnet(ctx, netProject, network, region)
		}
		if err != nil {
			return err
		}
	} else {
		vmSubnet = expandSubnet(vmSubnet, netProject, region)
		if needsPGA {
			if pgaErr := bastion.CheckPGA(ctx, vmSubnet); pgaErr != nil {
				return pgaErr
			}
		}
	}

	imageName, image, err := bastion.Image(ctx, cfg.Image)
	if err != nil {
		return err
	}

	vmName := bastion.Name()

	for _, h := range ext.Connect() {
		if h.BeforeBastion != nil {
			h.BeforeBastion(toExtAll(cfgs), explicit)
		}
	}

	spec := bastion.Spec{
		Project:        cfg.Project,
		Name:           vmName,
		NetworkProject: netProject,
		Network:        network,
		Subnet:         vmSubnet,
		Duration:       dur,
		Spot:           cfg.Spot,
		Tags:           tags,
		MachineType:    cfg.MachineType,
		Image:          image,
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	return runSession(ctx, session{
		cfg:       cfg,
		cfgs:      cfgs,
		targets:   targets,
		spec:      spec,
		zones:     zones,
		imageName: imageName,
		dur:       dur,
	}, realDeps)
}

func prepareConfigs(cfgs []connectConfig) (time.Duration, error) {
	var dur time.Duration
	for i := range cfgs {
		d, err := validateConfig(&cfgs[i])
		if err != nil {
			return 0, usage(stackMember(cfgs, i, err))
		}
		if err := validateValues(cfgs[i]); err != nil {
			return 0, usage(stackMember(cfgs, i, err))
		}
		if err := validateHooks(cfgs[i], nil); err != nil {
			return 0, usage(stackMember(cfgs, i, err))
		}
		if i == 0 {
			dur = d
		}
	}
	return dur, nil
}

func needsPrivateGoogleAccess(targets []target) bool {
	return slices.ContainsFunc(targets, func(t target) bool { return t.spec.NeedsPrivateGoogleAccess })
}

func firstError(chs []<-chan error) <-chan error {
	if len(chs) == 0 {
		return nil
	}
	out := make(chan error, 1)
	for _, ch := range chs {
		go func() {
			if err, ok := <-ch; ok && err != nil {
				select {
				case out <- err:
				default:
				}
			}
		}()
	}
	return out
}

func stackMember(cfgs []connectConfig, i int, err error) error {
	if len(cfgs) == 1 {
		return err
	}
	return fmt.Errorf("service %d of %d (%s %s): %w", i+1, len(cfgs), cfgs[i].Service, cfgs[i].Instance, err)
}

func validateConfig(cfg *connectConfig) (time.Duration, error) {
	if cfg.Duration == "" {
		cfg.Duration = "30m"
	}
	if cfg.Output == "" {
		cfg.Output = "text"
	}
	if cfg.Output != "text" && cfg.Output != "json" {
		return 0, fmt.Errorf("--output %q is not a format\nFix: use text or json", cfg.Output)
	}
	if cfg.StdinClose && len(cfg.Cmd) > 0 {
		return 0, errors.New("--end-on-stdin-close and -- CMD both use stdin\nFix: drop --end-on-stdin-close; the command ends the session when it exits")
	}
	if cfg.Output == "json" && len(cfg.Cmd) > 0 {
		return 0, fmt.Errorf("--output json and -- CMD both write to stdout\nFix: drop --output json; the command gets the address in BQTO_HOST, BQTO_PORT and BQTO_REMOTE")
	}
	if cfg.Service == "" {
		cfg.Service = "cloudsql"
	}
	if err := knownService(cfg.Service); err != nil {
		return 0, err
	}

	if cfg.Project == "" {
		cfg.Project = defaultProject()
	}

	if miss := missingFields(*cfg); len(miss) > 0 {
		return 0, fmt.Errorf("missing required flags: %s\nFix: add them; bqto connect --help lists every flag", strings.Join(miss, ", "))
	}

	dur, err := validateDuration(cfg.Duration)
	if err != nil {
		return 0, err
	}
	if cfg.Image != "" {
		if err := bastion.CheckImage(cfg.Image); err != nil {
			return 0, err
		}
	}
	return dur, nil
}

func openTunnel(tunnelCtx context.Context, supervised func() tunnel.Tunnel) (tunnel.Tunnel, error) {
	for attempt := 0; ; attempt++ {
		tun := supervised()
		startErr := tun.Start(tunnelCtx)
		if startErr == nil {
			return tun, nil
		}
		if !errors.Is(startErr, errInnerPortTaken) || attempt >= portRetries {
			return nil, fmt.Errorf("start tunnel: %w", startErr)
		}
	}
}

type failedTunnel struct {
	err  error
	done chan struct{}
}

func (f failedTunnel) Start(context.Context) error { return f.err }
func (f failedTunnel) Done() <-chan struct{}       { return f.done }
func (f failedTunnel) Stop() error                 { return nil }
func (f failedTunnel) Stderr() []string            { return nil }

var errInnerPortTaken = errors.New("another program took the tunnel's loopback port while the tunnel started")

type portRouter struct {
	mu     sync.Mutex
	holds  []*tunnel.Hold
	last   int
	routed int
}

func (r *portRouter) open(targets []target, innerPort func() (int, error), newTunnel func([]tunnel.Forward) tunnel.Tunnel) tunnel.Tunnel {
	ports := make([]int, len(targets))
	fwds := make([]tunnel.Forward, len(targets))
	for i := range targets {
		p, err := innerPort()
		if err != nil {
			return failedTunnel{err: err, done: make(chan struct{})}
		}
		ports[i] = p
		fwds[i] = targets[i].fwd
		fwds[i].LocalPort = p
	}
	r.mu.Lock()
	r.last++
	gen := r.last
	r.mu.Unlock()
	return &routedTunnel{Tunnel: newTunnel(fwds), router: r, gen: gen, ports: ports}
}

func (r *portRouter) up(gen int, ports []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routed = gen
	for i, h := range r.holds {
		h.Route(ports[i]) //nolint:gosec // open builds ports with one entry per hold
	}
}

func (r *portRouter) down(gen int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routed != gen {
		return
	}
	r.routed = 0
	for _, h := range r.holds {
		h.Route(0)
	}
}

type routedTunnel struct {
	tunnel.Tunnel
	router *portRouter
	gen    int
	ports  []int
}

func (t *routedTunnel) Start(ctx context.Context) error {
	err := t.Tunnel.Start(ctx)
	if errors.Is(err, tunnel.ErrPortTaken) {
		return fmt.Errorf("%w\nFix: run bqto connect again", errInnerPortTaken)
	}
	if err != nil {
		return err
	}
	t.router.up(t.gen, t.ports)
	go func() {
		<-t.Done()
		t.router.down(t.gen)
	}()
	return nil
}

func commandEnv(t target, localPort int) []string {
	env := []string{"BQTO_HOST=127.0.0.1", "BQTO_PORT=" + strconv.Itoa(localPort)}
	if t.spec.Env != nil {
		env = append(env, t.spec.Env(localPort)...)
	}
	return env
}

func findCommand(cmd []string) error {
	if len(cmd) == 0 {
		return nil
	}
	_, err := exec.LookPath(cmd[0])
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		return exitCodeError{code: exitCmdNotFound, err: fmt.Errorf("command %q not found\nFix: check the name after --, or give its full path", cmd[0])}
	default:
		return exitCodeError{code: exitCmdNotRunnable, err: fmt.Errorf("command %q cannot run: %w\nFix: make it executable with chmod +x, or give the path to the program itself", cmd[0], err)}
	}
}

func runCommand(cfg connectConfig, env []string, tun tunnel.Tunnel, sessionEnd func() error, auxErr <-chan error) error {
	ext.Debugf("command: %s with %d arguments (not shown: they may hold secrets)", cfg.Cmd[0], len(cfg.Cmd)-1)
	c := exec.Command(cfg.Cmd[0], cfg.Cmd[1:]...) //nolint:gosec // running the user's command after -- is the feature
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = append(os.Environ(), env...)
	if startErr := c.Start(); startErr != nil {
		if errors.Is(startErr, exec.ErrNotFound) || errors.Is(startErr, fs.ErrNotExist) {
			return exitCodeError{code: exitCmdNotFound, err: startErr}
		}
		return startErr
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, sessionSignals...)
	defer signal.Stop(sigCh)

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	tunDone := tun.Done()
	for {
		select {
		case sig := <-sigCh:
			if c.Process != nil && (sig == syscall.SIGTERM || !terminalForeground()) {
				_ = c.Process.Signal(sig)
			}
		case <-tunDone:
			fmt.Fprintln(os.Stderr, "session ended:", sessionEnd())
			tunDone = nil
		case err := <-auxErr:
			fmt.Fprintln(os.Stderr, "error:", err)
			auxErr = nil
		case runErr := <-done:
			if runErr != nil {
				if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
					code := exitErr.ExitCode()
					if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
						code = 128 + int(ws.Signal())
					}
					return exitCodeError{code: code}
				}
				return runErr
			}
			return nil
		}
	}
}

type readyEvent struct {
	Event    string `json:"event"`
	Service  string `json:"service"`
	Instance string `json:"instance"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Proxy    string `json:"proxy,omitempty"`
	Remote   string `json:"remote,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
	Auth     string `json:"auth,omitempty"`
	Extra    map[string]any
	Bastion  string
	Expires  string
}

type readyEventHead struct {
	Event    string `json:"event"`
	Service  string `json:"service"`
	Instance string `json:"instance"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Proxy    string `json:"proxy,omitempty"`
	Remote   string `json:"remote,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

type readyEventTail struct {
	Bastion string `json:"bastion"`
	Expires string `json:"expires"`
}

func (e readyEvent) MarshalJSON() ([]byte, error) {
	head, err := json.Marshal(readyEventHead{e.Event, e.Service, e.Instance, e.Host, e.Port, e.Proxy, e.Remote, e.TLS, e.Auth})
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(head[:len(head)-1])
	for _, k := range slices.Sorted(maps.Keys(e.Extra)) {
		key, _ := json.Marshal(k)
		val, err := json.Marshal(e.Extra[k])
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	tail, err := json.Marshal(readyEventTail{e.Bastion, e.Expires})
	if err != nil {
		return nil, err
	}
	buf.WriteByte(',')
	buf.Write(tail[1:])
	return buf.Bytes(), nil
}

func remoteOf(t target) (addr string, tls bool, auth string) {
	if t.spec.Remote != "" || t.fwd.RemoteHost == "" {
		return t.spec.Remote, t.spec.TLS, t.spec.Auth
	}
	return net.JoinHostPort(t.fwd.RemoteHost, strconv.Itoa(t.fwd.RemotePort)), t.spec.TLS, t.spec.Auth
}

func readyEvents(targets []target, ports []int, vmName string, deadline time.Time) []readyEvent {
	events := make([]readyEvent, len(targets))
	for i, t := range targets {
		ev := readyEvent{
			Event: "ready", Service: t.service, Instance: t.instanceRef,
			Host: "127.0.0.1", Port: ports[i],
			Bastion: vmName, Expires: deadline.UTC().Format(time.RFC3339),
		}
		ev.Remote, ev.TLS, ev.Auth = remoteOf(t)
		if t.fwd.RemoteHost == "" {
			ev.Proxy = "socks5"
		}
		for _, extra := range []map[string]any{t.spec.JSON, t.started.JSON} {
			for k, v := range extra {
				if ev.Extra == nil {
					ev.Extra = map[string]any{}
				}
				ev.Extra[k] = v
			}
		}
		events[i] = ev
	}
	return events
}

var defaultProject = auth.DefaultProject

type session struct {
	cfg       connectConfig
	cfgs      []connectConfig
	targets   []target
	spec      bastion.Spec
	zones     []string
	imageName string
	dur       time.Duration
}

type sessionDeps struct {
	create    func(ctx context.Context, s bastion.Spec, zones []string, onRetry func(zone, next string)) (string, error)
	delete    func(ctx context.Context, project, zone, name string, mayExist bool) (bool, error)
	status    func(ctx context.Context, project, zone, name string) (string, bool, error)
	hasSudo   func(ctx context.Context, project, zone, name string) (bool, error)
	vmID      func(ctx context.Context, project, zone, name string) (uint64, error)
	forget    func(project, zone string, id uint64) error
	newTunnel func(project, zone, name string, fwds []tunnel.Forward) tunnel.Tunnel
	listen    func(port int) (net.Listener, error)
	innerPort func() (int, error)
	reserve   func(want int) (int, func(), error)
	now       func() time.Time
}

var realDeps = sessionDeps{
	create:    bastion.CreateInZones,
	delete:    bastion.Delete,
	status:    bastion.Status,
	hasSudo:   bastion.HasSudo,
	vmID:      bastion.ID,
	forget:    forgetBastion,
	newTunnel: tunnel.Gcloud,
	listen:    tunnel.Listen,
	innerPort: tunnel.InnerPort,
	reserve:   reservePort,
	now:       time.Now,
}

func reservePort(want int) (int, func(), error) {
	return tunnel.ReservePort(tunnel.ReservedPortsDir(), want)
}

func forgetBastion(project, zone string, id uint64) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return tunnel.ForgetBastion(home, project, zone, id)
}

func runSession(ctx context.Context, s session, d sessionDeps) (err error) {
	cfg, cfgs, targets, spec, zones, imageName, dur := s.cfg, s.cfgs, s.targets, s.spec, s.zones, s.imageName, s.dur
	vmName := spec.Name
	fixed := fixedPorts(cfgs)
	held, ports, err := holdPorts(wantedPorts(cfgs, targets), fixed, d.listen)
	if err != nil {
		return err
	}
	router := &portRouter{holds: make([]*tunnel.Hold, len(held))}
	for i, ln := range held {
		router.holds[i] = tunnel.NewHold(ln)
	}
	var releaseOnce sync.Once
	releasePorts := func() {
		releaseOnce.Do(func() {
			for _, h := range router.holds {
				_ = h.Close()
			}
		})
	}
	defer releasePorts()
	for i := range targets {
		if want := wantedPorts(cfgs, targets)[i]; ports[i] != want {
			fmt.Fprintf(os.Stderr, "warn: port %d taken, using %d instead\n", want, ports[i])
		}
		targets[i].fwd.LocalPort = ports[i]
	}

	var vmZone string
	var vmID uint64
	var createErr error
	defer func() {
		mayExist := bastion.MayExistAfter(createErr)
		fmt.Fprintln(os.Stderr, cleanupStartMessage(mayExist))
		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer delCancel()
		requested, delErr := d.delete(delCtx, cfg.Project, vmZone, vmName, mayExist)
		if msg := cleanupMessage(cfg.Project, vmName, mayExist, requested, delErr); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		if vmID != 0 && delErr == nil {
			if forgetErr := d.forget(cfg.Project, vmZone, vmID); forgetErr != nil {
				fmt.Fprintf(os.Stderr, "warn: could not remove the bastion's host key and certificate from ~/.ssh: %v\n", forgetErr)
			}
		}
		var ec exitCodeError
		if delErr != nil && !errors.As(err, &ec) {
			err = exitCodeError{code: exitError, err: err}
		}
	}()

	fmt.Fprintf(os.Stderr, "creating bastion from %s…\n", imageName)
	vmZone, createErr = d.create(ctx, spec, zones, func(zone, next string) {
		fmt.Fprintf(os.Stderr, "zone %s is out of capacity, trying %s…\n", zone, next)
	})
	if createErr != nil {
		if _, ok := errors.AsType[*gcperr.Error](createErr); ok {
			return createErr
		}
		return fmt.Errorf("create bastion: %w", createErr)
	}
	fmt.Fprintf(os.Stderr, "bastion %s is in %s\n", vmName, vmZone)
	idCtx, idCancel := context.WithTimeout(ctx, 10*time.Second)
	vmID, err = d.vmID(idCtx, cfg.Project, vmZone, vmName)
	idCancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, vmIDWarning(err))
		err = nil
	}
	deadline := d.now().Add(dur)

	sudoCtx, sudoCancel := context.WithTimeout(ctx, 10*time.Second)
	sudo, sudoErr := d.hasSudo(sudoCtx, cfg.Project, vmZone, vmName)
	sudoCancel()
	switch {
	case sudoErr != nil:
		msg, _ := gcperr.SplitFix(sudoErr)
		fmt.Fprintf(os.Stderr, "warn: %s\n", msg)
	case sudo:
		fmt.Fprintln(os.Stderr, "warn: your roles give you sudo on the bastion (compute.instances.osAdminLogin); bqto only needs roles/compute.osLogin")
	}

	fmt.Fprintln(os.Stderr, "opening tunnel…")
	bastionStatus := func() (string, bool, error) {
		stCtx, stCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stCancel()
		return d.status(stCtx, cfg.Project, vmZone, vmName)
	}
	supervised := func() tunnel.Tunnel {
		return tunnel.Supervisor{
			New: func() tunnel.Tunnel {
				return router.open(targets, d.innerPort, func(fwds []tunnel.Forward) tunnel.Tunnel {
					return d.newTunnel(cfg.Project, vmZone, vmName, fwds)
				})
			},
			Alive: func() bool {
				status, found, stErr := bastionStatus()
				return canReconnect(stErr, found, status, d.now(), deadline)
			},
			OnRetry: func(attempt int, wait time.Duration) {
				fmt.Fprintf(os.Stderr, "warn: the tunnel dropped; reconnecting in %s (attempt %d)…\n", wait, attempt)
			},
			OnUp: func() {
				fmt.Fprintln(os.Stderr, "reconnected: the ports are open again")
			},
		}.Tunnel()
	}

	tunnelCtx, tunnelCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer tunnelCancel()
	tun, err := openTunnel(tunnelCtx, supervised)
	if err != nil {
		return err
	}
	defer func() {
		_ = tun.Stop()
		releasePorts()
	}()

	if delay, ok := warnDelay(d.now(), deadline); ok {
		warnTimer := time.AfterFunc(delay, func() {
			fmt.Fprintf(os.Stderr, "warn: the bastion stops at %s, in 5 minutes\n", deadline.Format("15:04"))
		})
		defer warnTimer.Stop()
	}
	sessionEnd := func() error {
		status, found, stErr := bastionStatus()
		return endReason(stErr, found, status, cfg.Spot, d.now(), deadline, cfg.Duration, tun.Stderr(), tunnel.LastStartError(tun))
	}
	expiry := fmt.Sprintf("expires at %s (%s)", deadline.Format("15:04"), timeLeft(d.now(), deadline))

	if len(targets) > 1 {
		fmt.Fprintf(os.Stderr, "ready: bastion %s, %s\n", vmName, expiry)
	}
	var auxDone []<-chan error
	for i := range targets {
		if targets[i].spec.Start != nil {
			started, startErr := targets[i].spec.Start(ctx, targets[i].ready(i, ports[i], d))
			if startErr != nil {
				return startErr
			}
			targets[i].started = started
			if started.Close != nil {
				defer started.Close()
			}
			if started.Done != nil {
				auxDone = append(auxDone, started.Done)
			}
		}
		for _, line := range readyLines(targets[i], targets[i].ready(i, ports[i], d), vmName, expiry, len(targets) > 1) {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	auxErr := firstError(auxDone)

	if cfg.Output == "json" {
		enc := json.NewEncoder(os.Stdout)
		for _, ev := range readyEvents(targets, ports, vmName, deadline) {
			if err := enc.Encode(ev); err != nil {
				return fmt.Errorf("write the ready event: %w\nFix: keep bqto's stdout open and read it until bqto exits", err)
			}
		}
	}

	if len(cfg.Cmd) > 0 {
		remote, needsTLS, credential := remoteOf(targets[0])
		tlsFlag := ""
		if needsTLS {
			tlsFlag = "1"
		}
		env := commandEnv(targets[0], ports[0])
		env = append(env, "BQTO_TLS="+tlsFlag, "BQTO_AUTH="+credential)
		if remote != "" {
			env = append(env, "BQTO_REMOTE="+remote)
		}
		env = append(env, targets[0].started.Env...)
		return runCommand(cfg, env, tun, sessionEnd, auxErr)
	}

	select {
	case <-ctx.Done():
		return nil
	case <-tun.Done():
		if ctx.Err() != nil {
			return nil
		}
		err := sessionEnd()
		if _, ok := errors.AsType[sessionExpired](err); ok {
			fmt.Fprintln(os.Stderr, "session ended:", err)
			return nil
		}
		return fmt.Errorf("session ended: %w", err)
	case err := <-auxErr:
		return fmt.Errorf("%w\nFix: run bqto connect again", err)
	}
}
