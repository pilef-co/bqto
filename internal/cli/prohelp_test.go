package cli

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
)

func runMain(t *testing.T, kind string, args ...string) (string, string, int) {
	t.Helper()
	c := helper(t, kind)
	c.Args = append(c.Args, append([]string{"--"}, args...)...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), c.ProcessState.ExitCode()
}

var proHelpArgs = [][]string{
	{"profile", "-h"},
	{"profile", "--help"},
	{"profile", "-help"},
	{"profile", "help"},
	{"profile", "create", "db", "--help"},
	{"help", "profile"},
	{"-h", "profile"},
	{"--help", "profile"},
	{"stack", "-h"},
	{"stack", "--help"},
	{"stack", "create", "-h"},
	{"help", "stack"},
}

func TestProHelpGolden(t *testing.T) {
	golden(t, "pro-help", ext.ProHelp())
}

func TestProCommandsShareOneHelpThatExitsZero(t *testing.T) {
	for _, kind := range []string{"main", "main-hidden"} {
		want := map[string]string{"main": "bqto Pro: ", "main-hidden": "Saved connections "}[kind]
		var first string
		for _, args := range proHelpArgs {
			out, errOut, code := runMain(t, kind, args...)
			if code != 0 || errOut != "" {
				t.Errorf("%s bqto %v: exit %d, stderr %q; want exit 0 and no stderr", kind, args, code, errOut)
			}
			if !strings.HasPrefix(out, want) {
				t.Errorf("%s bqto %v printed %q, want the shared help starting %q", kind, args, out, want)
			}
			if first == "" {
				first = out
			} else if out != first {
				t.Errorf("%s bqto %v printed a different help:\n%s\nwant:\n%s", kind, args, out, first)
			}
		}
	}
}

func TestProCommandsWithoutHelpStillNeedPro(t *testing.T) {
	tests := []struct {
		kind string
		args []string
		want string
	}{
		{"main", []string{"profile"}, "error: bqto Pro is needed for profiles\nFix: use bqto connect --service S --instance I, or join the bqto Pro beta at https://bqto.dev/#waitlist\n"},
		{"main", []string{"stack", "list"}, "error: bqto Pro is needed for stacks\nFix: use bqto connect --service S --instance I, or join the bqto Pro beta at https://bqto.dev/#waitlist\n"},
		{"main", []string{"profile", "create", "db", "--project", "help"}, "error: bqto Pro is needed for profiles\nFix: use bqto connect --service S --instance I, or join the bqto Pro beta at https://bqto.dev/#waitlist\n"},
		{"main-hidden", []string{"stack", "show", "work"}, "error: stacks are not available in this build\nFix: use bqto connect --service S --instance I\n"},
	}
	for _, tc := range tests {
		out, errOut, code := runMain(t, tc.kind, tc.args...)
		if code != exitUsage || out != "" || errOut != tc.want {
			t.Errorf("%s bqto %v: exit %d, stdout %q, stderr %q; want exit %d and %q", tc.kind, tc.args, code, out, errOut, exitUsage, tc.want)
		}
	}
}

func TestARegisteredProCommandKeepsItsOwnHelp(t *testing.T) {
	for _, args := range proHelpArgs {
		out, _, code := runMain(t, "main-pro", args...)
		if code != 0 || !strings.Contains(out, "'s own help") || strings.Contains(out, "saved connections") {
			t.Errorf("bqto Pro %v: exit %d, printed %q; want the registered command's own help", args, code, out)
		}
	}
	out, _, _ := runMain(t, "main-pro", "-h")
	if strings.Contains(out, "(bqto Pro)") || strings.Contains(out, "bqto profile COMMAND") {
		t.Errorf("bqto Pro -h lists the free placeholders:\n%s", out)
	}
}

func TestTheProBinaryListsItsFlagsLastUnderTheirOwnHeading(t *testing.T) {
	_, help, code := runMain(t, "main-pro", "connect", "-h")
	if code != 0 {
		t.Fatalf("connect -h exited %d:\n%s", code, help)
	}
	free, pro, found := strings.Cut(help, "\n\nbqto Pro flags:\n")
	if !found {
		t.Fatalf("connect -h has no bqto Pro flags group:\n%s", help)
	}
	if strings.Contains(help, ext.ProTag()) {
		t.Errorf("the Pro binary marks its own flags %s:\n%s", ext.ProTag(), help)
	}
	for _, f := range ext.ProFlags() {
		if !strings.Contains(pro, "  --"+f.Name+" ") && !strings.Contains(pro, "  --"+f.Name+"\n") {
			t.Errorf("--%s is not in the bqto Pro group:\n%s", f.Name, pro)
		}
		if strings.Contains(free, "  --"+f.Name+" ") || strings.Contains(free, "  --"+f.Name+"\n") {
			t.Errorf("--%s is among the flags every user has:\n%s", f.Name, free)
		}
	}
	for _, name := range []string{"--project", "--service", "--instance", "--end-on-stdin-close"} {
		if !strings.Contains(free, "  "+name+" ") && !strings.Contains(free, "  "+name+"\n") {
			t.Errorf("%s is not among the flags every user has:\n%s", name, free)
		}
	}
}

func TestTheFreeBinaryListsTheProFlagsLastWithoutAHeading(t *testing.T) {
	_, help, _ := runMain(t, "main", "connect", "-h")
	if strings.Contains(help, "bqto Pro flags:") {
		t.Errorf("the free binary names bqto Pro outside the %s mark:\n%s", ext.ProTag(), help)
	}
	free, pro, found := strings.Cut(help, "\n\n\n")
	if !found || strings.Contains(free, ext.ProTag()) {
		t.Fatalf("the flags marked %s are not in a group of their own after two blank lines:\n%s", ext.ProTag(), help)
	}
	for block := range strings.SplitSeq(strings.Trim(pro, "\n"), "\n\n") {
		_, text, _ := strings.Cut(block, "\n")
		if !strings.HasSuffix(strings.Join(strings.Fields(text), " "), " "+ext.ProTag()) {
			t.Errorf("%q in the last group is not marked %s", block, ext.ProTag())
		}
	}
}

func TestTheHelpShowsTheProExamplesOnlyInTheProBinary(t *testing.T) {
	free, _, _ := runMain(t, "main", "-h")
	pro, _, _ := runMain(t, "main-pro", "-h")
	if strings.Contains(free, "--save db") {
		t.Errorf("the free bqto -h shows examples that it refuses:\n%s", free)
	}
	if !strings.Contains(pro, "Examples:\n") || !strings.Contains(pro, ext.ProExamples()) {
		t.Errorf("the Pro bqto -h does not show the examples of profiles and stacks:\n%s", pro)
	}
	for _, out := range []string{free, pro} {
		if !strings.Contains(out, "-- psql -U app\n") {
			t.Errorf("bqto -h does not show the connect examples:\n%s", out)
		}
		for l := range strings.SplitSeq(out, "\n") {
			if len(l) > 80 {
				t.Errorf("line %q is %d columns, want at most 80", l, len(l))
			}
		}
	}
}

func TestTheConnectExamplesParse(t *testing.T) {
	examples := slices.Clone(connectExamples)
	for l := range strings.SplitSeq(ext.ProExamples(), "\n") {
		if command := strings.TrimSpace(l); strings.HasPrefix(command, "bqto connect ") {
			examples = append(examples, [2]string{command, ""})
		}
	}
	for _, e := range examples {
		args := strings.Fields(strings.TrimPrefix(e[0], "bqto connect "))
		_, flagArgs, _ := splitConnectArgs(args)
		var cfg ext.Config
		fs := flag.NewFlagSet("connect", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		registerConnectFlags(fs, &cfg)
		if err := fs.Parse(flagArgs); err != nil || fs.NArg() != 0 {
			t.Errorf("%q: %v, %v left over", e[0], err, fs.Args())
		}
	}
}

func TestEveryFlagHelpLineFitsEightyColumns(t *testing.T) {
	for _, kind := range []string{"main", "main-hidden", "main-pro"} {
		_, help, _ := runMain(t, kind, "connect", "-h")
		for l := range strings.SplitSeq(help, "\n") {
			if n := len(strings.Replace(l, "    \t", "        ", 1)); n > 80 {
				t.Errorf("%s connect -h: %q is %d columns, want at most 80", kind, l, n)
			}
		}
		if !strings.Contains(help, "\n\n  --duration ") {
			t.Errorf("%s connect -h has no blank line between flags:\n%s", kind, help)
		}
	}
}

func TestAFlagSetOfOnlyProFlagsHasNoGroup(t *testing.T) {
	fs := flag.NewFlagSet("stack create", flag.ContinueOnError)
	fs.String("profile", "", "a profile to include")
	var out bytes.Buffer
	printFlags(&out, fs)
	if got, want := out.String(), "  --profile string\n    \ta profile to include\n"; got != want {
		t.Errorf("printFlags = %q, want %q", got, want)
	}
}
