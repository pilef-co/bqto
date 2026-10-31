package ext

import (
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func restoreProAvailable(t *testing.T) {
	t.Helper()
	before := ProAvailable
	t.Cleanup(func() { ProAvailable = before })
}

func TestBqtoProIsAvailableByDefault(t *testing.T) {
	if !ProAvailable {
		t.Error("ProAvailable is false; the free bqto names bqto Pro")
	}
}

func TestProErrorNamesTheFeatureAndOneFix(t *testing.T) {
	restoreProAvailable(t)
	tests := []struct {
		available bool
		err       ProError
		want      string
	}{
		{false, ProError{What: "--web", Drop: "drop --web"}, "--web is not available in this build\nFix: drop --web"},
		{false, ProError{What: "Managed Kafka", Drop: "use another --service"}, "Managed Kafka is not available in this build\nFix: use another --service"},
		{false, ProError{What: "profiles", Drop: "use bqto connect --service S --instance I"}, "profiles are not available in this build\nFix: use bqto connect --service S --instance I"},
		{false, ProError{What: "stacks", Drop: "use bqto connect --service S --instance I"}, "stacks are not available in this build\nFix: use bqto connect --service S --instance I"},
		{true, ProError{What: "--web", Drop: "drop --web"}, "bqto Pro is needed for --web\nFix: drop --web, or join the bqto Pro beta at https://bqto.dev/#waitlist"},
		{true, ProError{What: "profiles", Drop: "use bqto connect --service S --instance I"}, "bqto Pro is needed for profiles\nFix: use bqto connect --service S --instance I, or join the bqto Pro beta at https://bqto.dev/#waitlist"},
	}
	for _, tc := range tests {
		ProAvailable = tc.available
		got := tc.err.Error()
		if got != tc.want {
			t.Errorf("ProAvailable=%v: ProError = %q, want %q", tc.available, got, tc.want)
		}
		if strings.Count(got, "\nFix: ") != 1 {
			t.Errorf("want exactly one Fix line: %q", got)
		}
	}
}

func TestProTagFollowsTheSwitch(t *testing.T) {
	restoreProAvailable(t)
	ProAvailable = false
	if got := ProTag(); got != "(not available)" {
		t.Errorf("ProTag = %q, want (not available) while bqto Pro is not available", got)
	}
	ProAvailable = true
	if got := ProTag(); got != "(bqto Pro)" {
		t.Errorf("ProTag = %q, want (bqto Pro) while bqto Pro is available", got)
	}
}

func TestEveryProCommandLabelIsPlural(t *testing.T) {
	for name, c := range proCommands {
		if !strings.HasSuffix(c.label, "s") {
			t.Errorf("ProCommand(%q) = %q; ProError says \"come\" for it, so it must be plural", name, c.label)
		}
	}
}

func TestProUsageListsUnregisteredProCommandsWithTheTag(t *testing.T) {
	restoreProAvailable(t)
	mu.Lock()
	before := commands
	commands = map[string]Command{}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		commands = before
		mu.Unlock()
	})

	tests := []struct {
		desc       string
		available  bool
		registered []string
		want       []string
	}{
		{"free, not available", false, nil, []string{
			"bqto profile COMMAND  saved connections (not available)",
			"bqto stack COMMAND  groups of profiles (not available)",
		}},
		{"free, available", true, nil, []string{
			"bqto profile COMMAND  saved connections (bqto Pro)",
			"bqto stack COMMAND  groups of profiles (bqto Pro)",
		}},
		{"stack registered", false, []string{"stack"}, []string{
			"bqto profile COMMAND  saved connections (not available)",
		}},
		{"bqto Pro registers both", true, []string{"profile", "stack"}, nil},
	}
	for _, tc := range tests {
		ProAvailable = tc.available
		mu.Lock()
		commands = map[string]Command{}
		for _, name := range tc.registered {
			commands[name] = Command{Name: name}
		}
		mu.Unlock()
		if got := ProUsage(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: ProUsage() = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

func TestProHelpFollowsProAvailable(t *testing.T) {
	restoreProAvailable(t)
	ProAvailable = false
	got := ProHelp()
	if strings.Contains(got, "bqto Pro") || !strings.HasPrefix(got, "Saved connections ") || !strings.HasSuffix(got, "\n\nThese commands are not available in this build.\n") {
		t.Errorf("ProHelp while not available:\n%s", got)
	}
	ProAvailable = true
	got = ProHelp()
	if !strings.HasPrefix(got, "bqto Pro: ") || !strings.HasSuffix(got, "\n\nThese commands need bqto Pro:\njoin the bqto Pro beta at https://bqto.dev/#waitlist.\n") {
		t.Errorf("ProHelp while available:\n%s", got)
	}
}

func TestProHelpFitsEightyColumnsInBothStates(t *testing.T) {
	restoreProAvailable(t)
	for _, available := range []bool{true, false} {
		ProAvailable = available
		lines := strings.Split(ProHelp(), "\n")
		for _, l := range lines {
			if len(l) > 80 {
				t.Errorf("ProAvailable=%v: line %q is %d columns, want at most 80", available, l, len(l))
			}
		}
		for i, e := range proExamples {
			command, about, gap := lines[1+3*i], lines[2+3*i], lines[3+3*i]
			if command != "  "+e[0] || about != "      "+e[1] || gap != "" {
				t.Errorf("ProAvailable=%v: example %d printed %q / %q / %q, want the command indented 2, its description under it indented 6, then a blank line", available, i, command, about, gap)
			}
		}
	}
}

func TestProJoinReplacesTheJoinText(t *testing.T) {
	restoreProAvailable(t)
	before := ProJoin
	t.Cleanup(func() { ProJoin = before })
	ProAvailable = true
	ProJoin = "see https://github.com/pilef-co/bqto for the latest news about bqto Pro"

	got := ProError{What: "--web", Drop: "drop --web"}.Error()
	want := "bqto Pro is needed for --web\nFix: drop --web, or see https://github.com/pilef-co/bqto for the latest news about bqto Pro"
	if got != want {
		t.Errorf("ProError = %q, want %q", got, want)
	}

	help := ProHelp()
	wantEnd := "\n\nThese commands need bqto Pro:\nsee https://github.com/pilef-co/bqto for the latest news about bqto Pro.\n"
	if !strings.HasSuffix(help, wantEnd) || strings.Contains(help, "bqto.dev/#waitlist") || strings.Contains(help, "join the bqto Pro beta") {
		t.Errorf("ProHelp with a replaced ProJoin:\n%s", help)
	}
	for l := range strings.SplitSeq(help, "\n") {
		if len(l) > 80 {
			t.Errorf("line %q is %d columns, want at most 80", l, len(l))
		}
	}

	ProAvailable = false
	if got := (ProError{What: "--web", Drop: "drop --web"}).Error() + ProHelp(); strings.Contains(got, "latest news") {
		t.Errorf("ProJoin shows while not available:\n%s", got)
	}
}

func TestUsageErrorUnwraps(t *testing.T) {
	inner := errors.New("bad")
	if err := (UsageError{Err: inner}); !errors.Is(err, inner) || err.Error() != "bad" {
		t.Errorf("UsageError = %v", err)
	}
	if got := Usagef("x %d", 1).Error(); got != "x 1" {
		t.Errorf("Usagef = %q", got)
	}
}

func TestEveryProFlagHasAUsage(t *testing.T) {
	for _, f := range ProFlags() {
		if f.Name == "" || f.Usage == "" {
			t.Errorf("flag %+v needs a name and a usage", f)
		}
	}
}

func TestServicesSortByOrderThenName(t *testing.T) {
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		delete(services, "ext-test-a")
		delete(services, "ext-test-b")
	})
	RegisterService(Service{Name: "ext-test-b", Order: 1000})
	RegisterService(Service{Name: "ext-test-a", Order: 1000})
	var names []string
	for _, s := range Services() {
		if strings.HasPrefix(s.Name, "ext-test-") {
			names = append(names, s.Name)
		}
	}
	if strings.Join(names, ",") != "ext-test-a,ext-test-b" {
		t.Errorf("order = %v", names)
	}
	defer func() {
		if recover() == nil {
			t.Error("registering a service twice must panic")
		}
	}()
	RegisterService(Service{Name: "ext-test-a"})
}

func TestConfigOptions(t *testing.T) {
	var c Config
	c.SetOption("web", "true")
	if c.Option("web") != "true" {
		t.Errorf("Option = %q", c.Option("web"))
	}
	c.SetOption("web", "")
	if _, set := c.Options["web"]; set {
		t.Error("an empty value must remove the option")
	}
}

func TestFlagRegistered(t *testing.T) {
	if FlagRegistered("bqto-test-flag") {
		t.Fatal("an unregistered flag is reported as registered")
	}
	RegisterConnect(ConnectHooks{TargetFlags: func(fs *flag.FlagSet, c *Config) {
		fs.StringVar(&c.Bastion.Subnet, "bqto-test-flag", "", "")
	}})
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		connectHooks = connectHooks[:len(connectHooks)-1]
	})
	if !FlagRegistered("bqto-test-flag") {
		t.Error("a flag from a TargetFlags hook is not reported as registered")
	}
}

func TestDebugfWritesOnlyWhenOn(t *testing.T) {
	t.Cleanup(func() { SetDebug(nil) })
	var buf strings.Builder
	Debugf("hidden %d", 1)
	SetDebug(&buf)
	if !Debugging() {
		t.Fatal("Debugging() = false after SetDebug")
	}
	Debugf("shown %d", 2)
	SetDebug(nil)
	Debugf("hidden %d", 3)
	if got := buf.String(); got != "debug: shown 2\n" {
		t.Errorf("debug output = %q", got)
	}
}

func TestDebugAndWarnLinesNeverContinueAProgressLine(t *testing.T) {
	oldOut := progressOut
	t.Cleanup(func() {
		progressOut = oldOut
		SetDebug(nil)
	})
	tests := []struct {
		desc  string
		debug bool
		warn  bool
		end   string
		want  string
	}{
		{"quiet, done", false, false, "done", "loading instances… done\n"},
		{"quiet, bare newline", false, false, "", "loading instances… \n"},
		{"debug, done", true, false, "done", "loading instances… \ndebug: google api: x took 1s\ndone\n"},
		{"debug, bare newline", true, false, "", "loading instances… \ndebug: google api: x took 1s\n"},
		{"warn, done", false, true, "done", "loading instances… \nwarn: list folders: denied\ndone\n"},
		{"warn, bare newline", false, true, "", "loading instances… \nwarn: list folders: denied\n"},
		{"debug and warn", true, true, "", "loading instances… \ndebug: google api: x took 1s\nwarn: list folders: denied\n"},
	}
	for _, tc := range tests {
		var buf strings.Builder
		progressOut = &buf
		SetDebug(nil)
		if tc.debug {
			SetDebug(&buf)
		}
		StartProgress("loading instances… ")
		Debugf("google api: %s took %s", "x", "1s")
		if tc.warn {
			Warnf("list folders: %s", "denied")
		}
		EndProgress(tc.end)
		Debugf("after")
		want := tc.want
		if tc.debug {
			want += "debug: after\n"
		}
		if got := buf.String(); got != want {
			t.Errorf("%s: output = %q, want %q", tc.desc, got, want)
		}
	}
}

func TestProUsageFitsEightyColumnsWhenAvailable(t *testing.T) {
	restoreProAvailable(t)
	ProAvailable = true
	for _, line := range ProUsage() {
		if n := utf8.RuneCountInString("  " + line); n > 80 {
			t.Errorf("%q is %d columns, want at most 80", line, n)
		}
	}
}
