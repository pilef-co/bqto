package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/selfupdate"
)

const (
	versionUsage = "Usage: bqto version [--output text|json]"
	updateUsage  = "Usage: bqto update"
)

func activeUpdater() (ext.Updater, bool) {
	if u, ok := ext.CustomUpdater(); ok {
		return u, true
	}
	have := buildVersion()
	gh := selfupdate.DefaultGitHub(have)
	return ext.Updater{
		Name:     "bqto",
		Line:     func() string { return "bqto " + have },
		Current:  func() string { return have },
		Timeout:  selfupdate.CheckTimeout,
		Latest:   gh.Latest,
		Download: gh.Fetch,
		DirFix:   "move bqto to a folder you own, such as ~/.local/bin, and put that folder on your PATH",
	}, false
}

func cmdVersion(args []string, u ext.Updater, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	output := fs.String("output", "text", "text, or json: version, latest, update_available and security on stdout")
	if help, err := parseFlags(fs, "bqto version", versionUsage, args); help || err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("bqto version takes no arguments, got %q\nFix: bqto version [--output json]", fs.Arg(0))
	}
	if *output != "text" && *output != "json" {
		return usagef("invalid value %q for flag --output\nFix: use --output text or --output json", *output)
	}
	if *output == "text" {
		fmt.Fprintln(stdout, u.Line())
	}
	st, err := selfupdate.Check(context.Background(), u)
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		ext.Debugf("could not check for updates: %s", first)
	}
	if *output == "json" {
		enc := json.NewEncoder(stdout)
		return enc.Encode(st)
	}
	selfupdate.Notice(stderr, u.Name, st)
	return nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	if help, err := parseFlags(fs, "bqto update", updateUsage, args); help || err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("bqto update takes no arguments, got %q\nFix: bqto update", fs.Arg(0))
	}
	u, custom := activeUpdater()
	exe, err := selfupdate.Executable()
	if err != nil {
		return err
	}
	bi, _ := debug.ReadBuildInfo()
	method := selfupdate.Detect(installOf(exe, bi, custom))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return selfupdate.Update(ctx, selfupdate.Options{
		Updater: u,
		Method:  method,
		Exe:     exe,
		Out:     os.Stdout,
		Err:     os.Stderr,
		Run:     selfupdate.RunTool,
		Sudo:    sudoHere(),
	})
}

func sudoHere() selfupdate.Sudo {
	_, err := exec.LookPath("sudo")
	return selfupdate.Sudo{
		Run:      selfupdate.RunTool,
		Terminal: isTerminal(os.Stdin),
		Root:     os.Geteuid() == 0,
		Found:    err == nil,
	}
}

func installOf(exe string, bi *debug.BuildInfo, custom bool) selfupdate.Install {
	return selfupdate.Install{
		Exe:        exe,
		Build:      bi,
		Stamped:    version != "dev" && version != "",
		GoBinDirs:  selfupdate.GoBinDirs,
		AllowTools: !custom,
	}
}
