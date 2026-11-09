package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/bastion"
	"github.com/pilef-co/bqto/internal/redis"
)

type connectConfig struct {
	Project     string
	Service     string
	Instance    string
	Endpoint    string
	Network     string
	Region      string
	Port        int
	Duration    string
	Spot        bool
	Zone        string
	Subnet      string
	MachineType string
	Image       string
	NoInput     bool
	StdinClose  bool
	Output      string
	Cmd         []string
	Options     map[string]string
}

func toExt(c connectConfig) ext.Config {
	return ext.Config{
		Project:         c.Project,
		Service:         c.Service,
		Instance:        c.Instance,
		Endpoint:        c.Endpoint,
		Network:         c.Network,
		Region:          c.Region,
		Zone:            c.Zone,
		Duration:        c.Duration,
		Output:          c.Output,
		Port:            c.Port,
		NoInput:         c.NoInput,
		Cmd:             c.Cmd,
		EndOnStdinClose: c.StdinClose,
		Bastion: ext.Bastion{
			Spot:        c.Spot,
			Subnet:      c.Subnet,
			MachineType: c.MachineType,
			Image:       c.Image,
		},
		Options: maps.Clone(c.Options),
	}
}

func fromExt(e ext.Config) connectConfig {
	return connectConfig{
		Project:     e.Project,
		Service:     e.Service,
		Instance:    e.Instance,
		Endpoint:    e.Endpoint,
		Network:     e.Network,
		Region:      e.Region,
		Zone:        e.Zone,
		Duration:    e.Duration,
		Output:      e.Output,
		Port:        e.Port,
		NoInput:     e.NoInput,
		StdinClose:  e.EndOnStdinClose,
		Cmd:         e.Cmd,
		Spot:        e.Bastion.Spot,
		Subnet:      e.Bastion.Subnet,
		MachineType: e.Bastion.MachineType,
		Image:       e.Bastion.Image,
		Options:     maps.Clone(e.Options),
	}
}

func toExtAll(cfgs []connectConfig) []ext.Config {
	out := make([]ext.Config, len(cfgs))
	for i, c := range cfgs {
		out[i] = toExt(c)
	}
	return out
}

func (c connectConfig) options() map[string]string {
	return maps.Clone(c.Options)
}

func splitConnectArgs(args []string) (profileName string, flagArgs []string, cmd []string) {
	head := args
	if sep := slices.Index(args, "--"); sep >= 0 {
		head, cmd = args[:sep], args[sep+1:]
	}

	if len(head) > 0 && !strings.HasPrefix(head[0], "-") {
		profileName = head[0]
		flagArgs = head[1:]
	} else {
		flagArgs = head
	}
	return
}

func registerConnectFlags(fs *flag.FlagSet, cfg *ext.Config) {
	registerTargetFlags(fs, cfg)
	fs.StringVar(&cfg.Output, "output", "text", "text, or json: one JSON object per service on stdout when the session is ready")
	fs.BoolVar(&cfg.NoInput, "no-input", false, "never prompt; fail on missing values (on when stdin is not a terminal, and with --output json)")
	fs.BoolVar(&cfg.EndOnStdinClose, "end-on-stdin-close", false, "end the session and delete the bastion, as on SIGTERM, when stdin closes; for an app that starts bqto and keeps a pipe to its stdin open (not with -- CMD)")
	fs.BoolFunc("debug", "print each Google API call, gcloud command and retry to stderr, for bug reports (also BQTO_DEBUG=1)", setDebug)
	for _, h := range ext.Connect() {
		if h.Flags != nil {
			h.Flags(fs, cfg)
		}
	}
	registerProPlaceholders(fs, false)
}

func registerTargetFlags(fs *flag.FlagSet, cfg *ext.Config) {
	if cfg.Options == nil {
		cfg.Options = map[string]string{}
	}
	fs.StringVar(&cfg.Project, "project", "", "GCP project ID")
	fs.StringVar(&cfg.Service, "service", "", "service type: "+serviceNames(func(ext.Service) bool { return true }))
	fs.StringVar(&cfg.Instance, "instance", "", instanceHelp())
	fs.StringVar(&cfg.Network, "network", "", "VPC network name or projects/P/global/networks/N (default: the instance's network)")
	fs.StringVar(&cfg.Region, "region", "", "region (inferred from instance if omitted)")
	portHelp := "local port (default: the service's port + 10000)"
	if slices.ContainsFunc(ext.Services(), func(s ext.Service) bool { _, pro := ext.ProService(s.Name); return pro }) {
		portHelp = "local port (default: the service's port + 10000, or 1080 for a SOCKS proxy)"
	}
	fs.IntVar(&cfg.Port, "p", 0, portHelp)
	fs.IntVar(&cfg.Port, "port", 0, portHelp)
	fs.StringVar(&cfg.Duration, "duration", "", "max VM lifetime from when the VM starts, e.g. 30m; the tunnel takes a minute or two to open (default: 30m)")
	fs.StringVar(&cfg.Zone, "zone", "", "zone override (default: first UP zone in region with capacity)")
	for _, h := range ext.Connect() {
		if h.TargetFlags != nil {
			h.TargetFlags(fs, cfg)
		}
	}
	registerProPlaceholders(fs, true)
}

type proPlaceholder struct{ isBool bool }

func (p proPlaceholder) String() string   { return "" }
func (p proPlaceholder) Set(string) error { return nil }
func (p proPlaceholder) IsBoolFlag() bool { return p.isBool }

func registerProPlaceholders(fs *flag.FlagSet, target bool) {
	for _, f := range ext.ProFlags() {
		if f.Target != target || fs.Lookup(f.Name) != nil {
			continue
		}
		fs.Var(proPlaceholder{isBool: f.Bool}, f.Name, f.Usage+" "+ext.ProTag())
	}
}

func proFlagUsed(fs *flag.FlagSet, explicit map[string]bool) error {
	for _, f := range ext.ProFlags() {
		fl := fs.Lookup(f.Name)
		if !explicit[f.Name] || fl == nil {
			continue
		}
		if _, placeholder := fl.Value.(proPlaceholder); placeholder {
			return ext.ProError{What: "--" + f.Name, Drop: "drop --" + f.Name}
		}
	}
	return nil
}

func parseFlags(fs *flag.FlagSet, command, usageLine string, args []string) (help bool, err error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err = fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, usageLine)
		printFlags(os.Stderr, fs)
		return true, nil
	}
	if err != nil {
		return false, usage(fmt.Errorf("%s\nFix: %s --help lists the flags", flagError(err), command))
	}
	return false, nil
}

var invalidFlagValue = regexp.MustCompile(`^invalid (boolean )?value (".*") for (?:flag )?-(\S+): (.*)$`)

func flagError(err error) string {
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		return "unknown flag " + dashed(name)
	}
	if name, ok := strings.CutPrefix(msg, "flag needs an argument: -"); ok {
		return "flag " + dashed(name) + " needs a value"
	}
	if m := invalidFlagValue.FindStringSubmatch(msg); m != nil {
		head := "invalid " + m[1] + "value " + m[2] + " for flag " + dashed(m[3])
		if strings.HasPrefix(m[4], "strconv.") {
			return head
		}
		return head + ": " + m[4]
	}
	return msg
}

func dashed(name string) string {
	name = strings.TrimLeft(name, "-")
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

func printFlags(w io.Writer, fs *flag.FlagSet) {
	var free, pro []*flag.Flag
	placeholders := false
	fs.VisitAll(func(f *flag.Flag) {
		if isProFlag(f.Name) {
			pro = append(pro, f)
			placeholders = placeholders || isPlaceholder(f)
			return
		}
		free = append(free, f)
	})
	if placeholders && len(free) > 0 {
		fmt.Fprint(w, joinFlags(fs, free, false)+"\n\n"+joinFlags(fs, pro, false))
		return
	}
	var all []*flag.Flag
	fs.VisitAll(func(f *flag.Flag) { all = append(all, f) })
	markPro := fs.Name() == "connect" && len(pro) > 0
	fmt.Fprint(w, joinFlags(fs, all, markPro))
	if markPro {
		fmt.Fprint(w, "\n"+proFlagMark+": bqto Pro\n")
	}
}

func joinFlags(fs *flag.FlagSet, flags []*flag.Flag, markPro bool) string {
	var blocks []string
	for _, f := range flags {
		if block := formatFlag(fs, f, markPro && isProFlag(f.Name)); block != "" {
			blocks = append(blocks, block)
		}
	}
	return strings.Join(blocks, "\n")
}

func isPlaceholder(f *flag.Flag) bool {
	_, placeholder := f.Value.(proPlaceholder)
	return placeholder
}

func isProFlag(name string) bool {
	return slices.ContainsFunc(ext.ProFlags(), func(f ext.ProFlag) bool { return f.Name == name })
}

func formatFlag(fs *flag.FlagSet, f *flag.Flag, proMark bool) string {
	if f.Name == "port" && fs.Lookup("p") != nil {
		return ""
	}
	name := "--" + f.Name
	if f.Name == "p" && fs.Lookup("port") != nil {
		name = "-p, --port"
	}
	kind, text := flag.UnquoteUsage(f)
	if proMark {
		text += "\x00" + proFlagMark
	}
	if p, placeholder := f.Value.(proPlaceholder); placeholder {
		kind = ""
		if !p.isBool {
			kind = "string"
		}
	}
	if kind == "value" {
		kind = "string"
	}
	if kind != "" {
		name += " " + kind
	}
	var b strings.Builder
	b.WriteString("  " + name + "\n")
	tag := ext.ProTag()
	glued := strings.ReplaceAll(tag, " ", "\x00")
	for _, line := range wrapWords(strings.ReplaceAll(text, tag, glued), flagTextWidth) {
		b.WriteString(flagTextIndent + strings.ReplaceAll(line, "\x00", " ") + "\n")
	}
	return b.String()
}

const (
	proFlagMark    = "(P)"
	flagTextIndent = "    \t"
	flagTextWidth  = 80 - 8
)

func wrapWords(text string, width int) []string {
	var lines []string
	line := ""
	for word := range strings.FieldsSeq(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) > width:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	return append(lines, line)
}

func noInput(cfg ext.Config, stdinIsTerminal bool) bool {
	return cfg.NoInput || cfg.EndOnStdinClose || cfg.Output == "json" || !stdinIsTerminal
}

var (
	stdinIsTerminal = func() bool { return isTerminal(os.Stdin) }
	wizard          = runWizard
)

func cmdConnect(args []string) error {
	positional, flagArgs, cmd := splitConnectArgs(args)

	ec := ext.Config{Cmd: cmd, Options: map[string]string{}}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	registerConnectFlags(fs, &ec)

	if help, err := parseFlags(fs, "bqto connect", connectUsageLine(), flagArgs); help || err != nil {
		return err
	}

	if rest := fs.Args(); len(rest) > 0 {
		if hasExpandHook() {
			return usagef("unexpected arguments %v\nFix: put the profile name before the flags and the command after --", rest)
		}
		return usagef("unexpected arguments %v\nFix: put the command after --", rest)
	}

	ec.NoInput = noInput(ec, stdinIsTerminal())

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if err := proFlagUsed(fs, explicit); err != nil {
		return usage(err)
	}
	if err := checkPortZero(explicit, ec.Port); err != nil {
		return usage(err)
	}

	cfgs, err := expandConfigs(positional, ec, explicit)
	if err != nil {
		return err
	}
	for i := range cfgs {
		cfgs[i].StdinClose = cfgs[i].StdinClose || ec.EndOnStdinClose
	}
	if len(cfgs) > 1 {
		return doConnect(cfgs, explicit)
	}
	cfg := cfgs[0]
	if cfg.Service != "" {
		if err := knownService(cfg.Service); err != nil {
			return usage(err)
		}
	}
	if err := validateHooks(cfg, explicit); err != nil {
		return usage(err)
	}

	if !cfg.NoInput && stdinIsTerminal() &&
		(cfg.Project == "" || cfg.Instance == "") {
		tuiCfg, err := wizard(context.Background(), cfg)
		if err != nil {
			return err
		}
		mergeWizardResult(&cfg, tuiCfg)
	}

	return doConnect([]connectConfig{cfg}, explicit)
}

func connectUsageLine() string {
	if hasExpandHook() {
		return "Usage: bqto connect [PROFILE] [flags] [-- CMD ARGS...]"
	}
	return "Usage: bqto connect [flags] [-- CMD ARGS...]"
}

func hasExpandHook() bool {
	return slices.ContainsFunc(ext.Connect(), func(h ext.ConnectHooks) bool { return h.Expand != nil })
}

func expandConfigs(positional string, ec ext.Config, explicit map[string]bool) ([]connectConfig, error) {
	for _, h := range ext.Connect() {
		if h.Expand == nil {
			continue
		}
		got, handled, err := h.Expand(positional, ec, explicit)
		if err != nil {
			return nil, err
		}
		if handled {
			out := make([]connectConfig, len(got))
			for i, c := range got {
				out[i] = fromExt(c)
			}
			return out, nil
		}
	}
	if positional != "" {
		label, _ := ext.ProCommand("profile")
		return nil, usage(ext.ProError{What: label, Drop: fmt.Sprintf("give --service and --instance instead of %q", positional)})
	}
	return []connectConfig{fromExt(ec)}, nil
}

func validateHooks(cfg connectConfig, explicit map[string]bool) error {
	for _, h := range ext.Connect() {
		if h.Validate == nil {
			continue
		}
		if err := h.Validate(toExt(cfg), explicit); err != nil {
			return err
		}
	}
	return nil
}

func mergeWizardResult(cfg *connectConfig, w connectConfig) {
	if cfg.Project == "" {
		cfg.Project = w.Project
	}
	if cfg.Service == "" {
		cfg.Service = w.Service
	}
	if cfg.Instance == "" {
		cfg.Instance = w.Instance
	}
	if cfg.Duration == "" {
		cfg.Duration = w.Duration
	}
	if !cfg.Spot {
		cfg.Spot = w.Spot
	}
	for k, v := range w.Options {
		if _, set := cfg.Options[k]; !set {
			if cfg.Options == nil {
				cfg.Options = map[string]string{}
			}
			cfg.Options[k] = v
		}
	}
}

func missingFields(cfg connectConfig) []string {
	var miss []string
	if cfg.Project == "" {
		miss = append(miss, "--project")
	}
	if cfg.Service == "" {
		miss = append(miss, "--service")
	}
	if cfg.Instance == "" {
		miss = append(miss, "--instance")
	}
	return miss
}

func checkPort(flagName string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d is not a port\nFix: use a port from 1 to 65535", flagName, port)
	}
	return nil
}

func checkPortZero(explicit map[string]bool, port int) error {
	if (explicit["p"] || explicit["port"]) && port == 0 {
		return errors.New("-p 0 is not a port\nFix: use a port from 1 to 65535, or leave -p out for the default")
	}
	return nil
}

func validateValues(cfg connectConfig) error {
	if cfg.Port != 0 {
		if err := checkPort("-p", cfg.Port); err != nil {
			return err
		}
	}
	if cfg.Network != "" {
		if _, _, err := parseNetworkRef(cfg.Network, cfg.Project); err != nil {
			return err
		}
	}
	if cfg.Subnet != "" {
		if err := bastion.CheckSubnet(cfg.Subnet); err != nil {
			return err
		}
	}
	if cfg.Service == "redis" {
		if err := redis.CheckRef(cfg.Instance); err != nil {
			return err
		}
	}
	return nil
}

const (
	minDuration = 30 * time.Second
	maxDuration = 120 * 24 * time.Hour
)

func validateDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--duration: %w\nFix: use a duration such as 30m, 2h or 1h30m", err)
	}
	if d < minDuration || d > maxDuration {
		return 0, fmt.Errorf("--duration %s is out of range\nFix: use a value from 30s to 2880h (120 days)", s)
	}
	return d, nil
}

func instanceHelp() string {
	var helps []string
	for _, s := range ext.Services() {
		helps = append(helps, s.RefHelp)
	}
	return joinOr(helps)
}
