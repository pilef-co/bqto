package cli

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
)

func TestCompletions(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{""}, []string{"completion", "connect", "help", "update", "version"}},
		{[]string{"co"}, []string{"completion", "connect"}},
		{[]string{"-"}, []string{"--debug"}},
		{[]string{"--de"}, []string{"--debug"}},
		{[]string{"--debug", ""}, []string{"completion", "connect", "help", "update", "version"}},
		{[]string{"--debug", "connect", "--se"}, []string{"--service"}},
		{[]string{"-debug=true", "connect", "--service", "c"}, []string{"cloudsql"}},
		{[]string{"--debug", "=", "true", "connect", "--se"}, []string{"--service"}},
		{[]string{"--debug", "--"}, nil},
		{[]string{"connect", "--se"}, []string{"--service"}},
		{[]string{"connect", "--service", "c"}, []string{"cloudsql"}},
		{[]string{"connect", "--service", "=", "va"}, []string{"valkey"}},
		{[]string{"connect", "--service=cl"}, []string{"cloudsql"}},
		{[]string{"connect", "--output=j"}, []string{"json"}},
		{[]string{"connect", "--output", ""}, []string{"json", "text"}},
		{[]string{"connect", "--project", ""}, nil},
		{[]string{"connect", "--project", "p", "--no-in"}, []string{"--no-input"}},
		{[]string{"connect", "--service", "cloudsql", "--output", "json", "--duration", "1h", "--network", "n", "--project", "p", "--instance", "i", "--region", "r", "--zone", "z", "-p", "1", "--no-in"}, []string{"--no-input"}},
		{[]string{"connect", "--no-input", "--zo"}, []string{"--zone"}},
		{[]string{"connect", "--sp"}, nil},
		{[]string{"connect", "--zone", "a", ":", ""}, nil},
		{[]string{"connect", "--", ""}, nil},
		{[]string{"completion", ""}, []string{"bash", "fish", "zsh"}},
		{[]string{"help", "con"}, []string{"connect"}},
		{[]string{"version", ""}, nil},
	}
	for _, tc := range tests {
		if got := completions(tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("completions(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestAnEmptyWordAfterAFlagListsTheFlags(t *testing.T) {
	for _, args := range [][]string{{"connect", ""}, {"connect", "--no-input", ""}, {"connect", "--service", "cloudsql", ""}} {
		if got := completions(args); !slices.Contains(got, "--project") || !slices.Contains(got, "-p") {
			t.Errorf("completions(%q) = %q, want the connect flags", args, got)
		}
	}
}

func TestEveryServiceCompletes(t *testing.T) {
	got := completions([]string{"connect", "--service", ""})
	for _, s := range ext.Services() {
		if !slices.Contains(got, s.Name) {
			t.Errorf("completions of --service %q miss %s", got, s.Name)
		}
	}
}

func TestCompletionScriptsParse(t *testing.T) {
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s is not installed", shell)
			}
			script := map[string]string{"bash": bashCompletion, "zsh": zshCompletion, "fish": fishCompletion}[shell]
			c := exec.Command(shell, "-n")
			c.Stdin = strings.NewReader(script)
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("%s -n: %v\n%s", shell, err, out)
			}
		})
	}
}

func fakeBqto(t *testing.T) []string {
	t.Helper()
	bin := t.TempDir()
	wrapper := "#!/bin/sh\nBQTO_TEST_HELPER=complete exec '" + os.Args[0] + "' -test.run=^TestHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "bqto"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
}

func TestShellCompletionCallsBqto(t *testing.T) {
	tests := []struct {
		shell, script, want string
	}{
		{"bash", bashCompletion + `COMP_WORDS=(bqto connect --service cl); COMP_CWORD=3; _bqto; printf '%s\n' "${COMPREPLY[@]}"`, "cloudsql"},
		{"bash", bashCompletion + `COMP_WORDS=(bqto connect --service = cl); COMP_CWORD=4; _bqto; printf '%s\n' "${COMPREPLY[@]}"`, "cloudsql"},
		{"bash", bashCompletion + `COMP_WORDS=(bqto --debug = true connect --service cl); COMP_CWORD=6; _bqto; printf '%s\n' "${COMPREPLY[@]}"`, "cloudsql"},
		{"fish", fishCompletion + `complete -C "bqto connect --service cl"`, "cloudsql"},
		{"fish", fishCompletion + `complete -C "bqto connect --service=cl"`, "--service=cloudsql"},
		{"fish", fishCompletion + `complete -C "bqto connect --output="`, "--output=json\n--output=text"},
	}
	for _, tc := range tests {
		t.Run(tc.shell+" "+tc.want, func(t *testing.T) {
			if _, err := exec.LookPath(tc.shell); err != nil {
				t.Skipf("%s is not installed", tc.shell)
			}
			c := exec.Command(tc.shell, "-c", tc.script)
			c.Env = fakeBqto(t)
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("%s completion = %q, want %q", tc.shell, got, tc.want)
			}
		})
	}
}

func TestZshCompletionWorksAutoloadedAndSourced(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "_bqto"), []byte(zshCompletion), 0o600); err != nil {
		t.Fatal(err)
	}
	stubs := `compset(){ :; }; compadd(){ print -l -- ${(P)2}; }; compdef(){ print -r -- compdef $*; }; `
	tests := []struct {
		name, script, want string
	}{
		{"autoloaded", stubs + `fpath=(` + dir + ` $fpath); autoload -Uz _bqto; words=(bqto connect --service cl); CURRENT=4; _bqto`, "cloudsql"},
		{"sourced", stubs + `source ` + filepath.Join(dir, "_bqto"), "compdef _bqto bqto"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := exec.Command("zsh", "-f", "-c", tc.script)
			c.Env = fakeBqto(t)
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("zsh %s = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestCompletionNeedsOneShell(t *testing.T) {
	for _, args := range [][]string{nil, {"powershell"}, {"bash", "zsh"}} {
		err := cmdCompletion(args)
		if err == nil || !strings.Contains(err.Error(), "\nFix: ") {
			t.Errorf("cmdCompletion(%q) = %v, want a usage error with a Fix line", args, err)
		}
	}
}

func TestTabNeverOffersWhatTheHelpMarksBqtoPro(t *testing.T) {
	offered := map[string]bool{}
	for _, args := range [][]string{{""}, {"help", ""}, {"connect", ""}, {"connect", "-"}, {"connect", "--service", ""}} {
		for _, c := range completions(args) {
			offered[c] = true
		}
	}
	if !offered["--project"] || !offered["cloudsql"] || !offered["connect"] {
		t.Fatalf("Tab offers %v, want at least connect, --project and cloudsql", offered)
	}
	for _, f := range ext.ProFlags() {
		if offered["--"+f.Name] {
			t.Errorf("Tab offers --%s, which the help marks %s", f.Name, ext.ProTag())
		}
	}
	for c := range offered {
		_, command := ext.ProCommand(c)
		_, service := ext.ProService(c)
		_, variant := ext.ProVariant(c)
		if command || service || variant {
			t.Errorf("Tab offers %s, which this binary refuses", c)
		}
	}
	for _, line := range ext.ProUsage() {
		if !strings.HasSuffix(line, " "+ext.ProTag()) {
			t.Errorf("bqto -h lists %q without the mark %s", line, ext.ProTag())
		}
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	registerConnectFlags(fs, &ext.Config{})
	for _, f := range ext.ProFlags() {
		if fl := fs.Lookup(f.Name); fl == nil || !strings.HasSuffix(fl.Usage, " "+ext.ProTag()) {
			t.Errorf("bqto connect -h does not mark --%s %s", f.Name, ext.ProTag())
		}
	}
}
