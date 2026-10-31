package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/mattn/go-isatty"
	"github.com/pilef-co/bqto/ext"
)

const (
	exitError          = 1
	exitUsage          = 2
	exitCmdNotRunnable = 126
	exitCmdNotFound    = 127
	exitInterrupted    = 130
)

func Main(args []string) int {
	if v := os.Getenv("BQTO_DEBUG"); v != "" {
		if err := setDebug(v); err != nil {
			fmt.Fprintf(os.Stderr, "warn: BQTO_DEBUG=%s is not 1, true, 0 or false, so debug output stays off\n", v)
		}
	}
	if err := run(args); err != nil {
		return report(os.Stderr, err)
	}
	return 0
}

func setDebug(value string) error {
	on, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	if on {
		ext.SetDebug(os.Stderr)
	} else {
		ext.SetDebug(nil)
	}
	return nil
}

func report(w io.Writer, err error) int {
	if errors.Is(err, huh.ErrUserAborted) {
		return exitInterrupted
	}
	if ec, ok := errors.AsType[exitCodeError](err); ok {
		if ec.err != nil {
			fmt.Fprintln(w, "error:", ec.err)
		}
		return ec.code
	}
	fmt.Fprintln(w, "error:", err)
	if isUsageError(err) {
		return exitUsage
	}
	return exitError
}

func isUsageError(err error) bool {
	if _, ok := errors.AsType[ext.UsageError](err); ok {
		return true
	}
	_, ok := errors.AsType[ext.ProError](err)
	return ok
}

type exitCodeError struct {
	code int
	err  error
}

func (e exitCodeError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e exitCodeError) Unwrap() error { return e.err }

func usage(err error) error {
	if err == nil {
		return nil
	}
	return exitCodeError{code: exitUsage, err: err}
}

func usagef(format string, a ...any) error {
	return usage(fmt.Errorf(format, a...))
}

func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

func run(args []string) error {
	args, err := withoutLeadingDebug(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		if !isTerminal(os.Stdin) {
			return usagef("no command given\nFix: %s; bqto -h shows the help", commandList())
		}
		return runInteractive()
	}
	switch args[0] {
	case "connect":
		return cmdConnect(args[1:])
	case "completion":
		return cmdCompletion(args[1:])
	case "__complete":
		return cmdComplete(args[1:])
	case "version", "--version", "-version":
		u, _ := activeUpdater()
		return cmdVersion(args[1:], u, os.Stdout, os.Stderr)
	case "update":
		return cmdUpdate(args[1:])
	case "--help", "-help", "-h", "help":
		if len(args) > 1 {
			if args[1] == "connect" {
				return cmdConnect([]string{"-h"})
			}
			if _, ok := ext.LookupCommand(args[1]); ok {
				return run([]string{args[1], "--help"})
			}
			if _, pro := ext.ProCommand(args[1]); pro {
				fmt.Print(ext.ProHelp())
				return nil
			}
		}
		printUsage()
		return nil
	}
	if c, ok := ext.LookupCommand(args[0]); ok {
		return c.Run(args[1:])
	}
	if label, pro := ext.ProCommand(args[0]); pro {
		if asksForHelp(args[1:]) {
			fmt.Print(ext.ProHelp())
			return nil
		}
		return usage(ext.ProError{What: label, Drop: "use bqto connect --service S --instance I"})
	}
	return usagef("unknown command %q\nFix: %s; bqto -h shows the help", args[0], commandList())
}

func asksForHelp(args []string) bool {
	if len(args) > 0 && args[0] == "help" {
		return true
	}
	return slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "-help" || a == "--help" })
}

func withoutLeadingDebug(args []string) ([]string, error) {
	for len(args) > 0 {
		name, value, hasValue := strings.Cut(args[0], "=")
		if name != "--debug" && name != "-debug" {
			break
		}
		if !hasValue {
			value = "true"
		}
		if err := setDebug(value); err != nil {
			return nil, usagef("invalid boolean value %q for flag --debug\nFix: use --debug alone, or --debug=true or --debug=false", value)
		}
		args = args[1:]
	}
	return args, nil
}

func commandList() string {
	names := []string{"bqto connect"}
	for _, c := range sortedCommands() {
		names = append(names, c.Name)
	}
	return strings.Join(append(names, "completion", "version", "update"), "|")
}

func sortedCommands() []ext.Command {
	cmds := ext.Commands()
	slices.SortFunc(cmds, func(a, b ext.Command) int { return strings.Compare(a.Name, b.Name) })
	return cmds
}

var version = "dev"

func buildVersion() string {
	var modVersion string
	if bi, ok := debug.ReadBuildInfo(); ok {
		modVersion = bi.Main.Version
	}
	return resolveVersion(version, modVersion)
}

func resolveVersion(ldflagsVersion, modVersion string) string {
	if ldflagsVersion != "dev" && ldflagsVersion != "" {
		return ldflagsVersion
	}
	if modVersion != "" && modVersion != "(devel)" {
		return modVersion
	}
	return "dev"
}

var connectExamples = [][2]string{
	{"bqto connect --project P --service cloudsql --instance orders", "open a tunnel; the ready: line shows the local port"},
	{"bqto connect --project P --service cloudsql --instance orders -- psql -U app", "run psql through the tunnel, then end the session"},
}

type usageLine struct {
	command string
	about   string
}

func parseUsageLine(line string) usageLine {
	command, about, _ := strings.Cut(line, "  ")
	return usageLine{command: strings.TrimSpace(command), about: strings.TrimSpace(about)}
}

func isConnectUsage(l usageLine) bool {
	return l.command == "bqto connect" || strings.HasPrefix(l.command, "bqto connect ")
}

func usageGroups() [][]usageLine {
	wizard := []usageLine{{"bqto", "start the interactive wizard"}}
	connect := []usageLine{{"bqto connect [flags] [-- CMD ARGS...]", "open a session; see bqto connect -h"}}
	var commands []usageLine
	for _, c := range sortedCommands() {
		for _, s := range c.Usage {
			l := parseUsageLine(s)
			switch {
			case l.command == "bqto":
				wizard = []usageLine{l}
			case isConnectUsage(l):
				connect = append(connect, l)
			default:
				commands = append(commands, l)
			}
		}
	}
	for _, s := range ext.ProUsage() {
		commands = append(commands, parseUsageLine(s))
	}
	other := []usageLine{
		{"bqto completion bash|zsh|fish", "print a shell completion script"},
		{"bqto version [--output json]", "show the version and any new release"},
		{"bqto update", "replace bqto with the newest release"},
	}
	return slices.DeleteFunc([][]usageLine{wizard, connect, commands, other}, func(g []usageLine) bool { return len(g) == 0 })
}

func formatUsage(groups [][]usageLine) string {
	width := 0
	for _, g := range groups {
		for _, l := range g {
			if l.about != "" {
				width = max(width, len(l.command))
			}
		}
	}
	blocks := make([]string, 0, len(groups))
	for _, g := range groups {
		var b strings.Builder
		for _, l := range g {
			b.WriteString(strings.TrimRight(fmt.Sprintf("  %-*s  %s", width, l.command, l.about), " ") + "\n")
		}
		blocks = append(blocks, b.String())
	}
	return strings.Join(blocks, "\n")
}

func formatExamples(examples [][2]string) string {
	blocks := make([]string, 0, len(examples))
	for _, e := range examples {
		blocks = append(blocks, fmt.Sprintf("  %s\n      %s\n", e[0], e[1]))
	}
	return strings.Join(blocks, "\n")
}

func printUsage() {
	fmt.Print("Usage:\n" + formatUsage(usageGroups()))
	examples := []string{formatExamples(connectExamples)}
	if len(ext.ProUsage()) == 0 {
		examples = append(examples, ext.ProExamples())
	}
	fmt.Print("\nExamples:\n" + strings.Join(examples, "\n"))
	fmt.Print("\nPut --debug before any command, or set BQTO_DEBUG=1, to print each Google API\ncall, gcloud command and retry to stderr. Run 'bqto connect --help' for connect\nflags.\n")
}

func runInteractive() error {
	for _, h := range ext.Connect() {
		if h.Start == nil {
			continue
		}
		ec, handled, err := h.Start(context.Background())
		if err != nil {
			return err
		}
		if handled {
			return doConnect([]connectConfig{fromExt(ec)}, map[string]bool{})
		}
	}
	cfg, err := runWizard(context.Background(), connectConfig{})
	if err != nil {
		return err
	}
	return doConnect([]connectConfig{cfg}, map[string]bool{})
}
