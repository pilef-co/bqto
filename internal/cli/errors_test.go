package cli

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/pilef-co/bqto/gcperr"
	"github.com/pilef-co/bqto/internal/tunnel"
)

var concat = regexp.MustCompile(`"\s*\+\s*"`)

func continues(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasSuffix(line, "Errorf(") || strings.HasSuffix(line, `"+`)
}

func TestOneFixFormat(t *testing.T) {
	other := regexp.MustCompile(`Fix with|Fix [A-Za-z]+:|cannot continue:|Install the Google|Request your network admin|: run\\n`)
	inError := regexp.MustCompile(`(Errorf|errors\.New|Sprintf|http\.Error)\(.*; (use|run|pass|pick|remove|try|ask|check|grant) `)
	root := filepath.Join("..", "..")
	scanned := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		scanned[filepath.ToSlash(rel)] = true
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			for j := i + 1; j < len(lines) && continues(line); j++ {
				line = concat.ReplaceAllString(line+lines[j], "")
			}
			m := other.FindString(line)
			if m == "" {
				cause, _, _ := strings.Cut(line, `\nFix: `)
				m = inError.FindString(cause)
			}
			if m != "" {
				t.Errorf("%s:%d uses %q; every fix starts with \"\\nFix: \"", path, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"cmd/bqto", "internal/cli", "gcperr", "internal/bastion", "internal/cloudsql"} {
		if !scanned[pkg] {
			t.Errorf("TestOneFixFormat did not scan %s", pkg)
		}
	}
}

func TestFlagErrorsWriteFlagsAsTheHelpDoes(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "unknown flag --bogus\nFix: bqto connect --help lists the flags"},
		{[]string{"-x"}, "unknown flag -x\nFix: bqto connect --help lists the flags"},
		{[]string{"--output"}, "flag --output needs a value\nFix: bqto connect --help lists the flags"},
		{[]string{"-p"}, "flag -p needs a value\nFix: bqto connect --help lists the flags"},
		{[]string{"--port", "abc"}, `invalid value "abc" for flag --port: parse error` + "\nFix: bqto connect --help lists the flags"},
		{[]string{"--debug=maybe"}, `invalid boolean value "maybe" for flag --debug` + "\nFix: bqto connect --help lists the flags"},
		{[]string{"--no-input=maybe"}, `invalid boolean value "maybe" for flag --no-input: parse error` + "\nFix: bqto connect --help lists the flags"},
	}
	for _, tc := range tests {
		err := cmdConnect(tc.args)
		if err == nil || err.Error() != tc.want {
			t.Errorf("cmdConnect(%q) = %q, want %q", tc.args, err, tc.want)
		}
	}
}

func TestABadFlagIsReportedOnce(t *testing.T) {
	err := cmdConnect([]string{"-bogus"})
	want := "unknown flag --bogus\nFix: bqto connect --help lists the flags"
	if err == nil || err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if err := cmdConnect([]string{"-h"}); err != nil {
		t.Errorf("connect -h: %v", err)
	}
	if err := run([]string{"help", "connect"}); err != nil {
		t.Errorf("help connect: %v", err)
	}
}

func checkOneFix(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error", name)
		return
	}
	msg := err.Error()
	if n := strings.Count(msg, "\nFix: "); n != 1 {
		t.Errorf("%s: %d Fix lines, want 1:\n%s", name, n, msg)
		return
	}
	_, fix, _ := strings.Cut(msg, "\nFix: ")
	lines := strings.Split(fix, "\n")
	if strings.TrimSpace(lines[0]) == "" {
		t.Errorf("%s: the Fix is empty:\n%s", name, msg)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, " ") {
			t.Errorf("%s: %q follows the Fix and is not part of it:\n%s", name, l, msg)
		}
	}
}

func TestEveryErrorEndsWithOneFix(t *testing.T) {
	old := defaultProject
	defaultProject = func() string { return "p" }
	t.Cleanup(func() { defaultProject = old })

	validate := func(cfg connectConfig) error {
		_, err := validateConfig(&cfg)
		return err
	}
	taken := func(int) (net.Listener, error) { return nil, errors.New("in use") }
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	ended := func(statusErr error, found bool, status string, spot bool, at time.Time, stderr []string, lastStart error) error {
		return endReason(statusErr, found, status, spot, at, later, "1h", stderr, lastStart)
	}
	two := []connectConfig{{Service: "cloudsql", Instance: "db"}, {Service: "redis", Instance: "r/c"}}
	_, _, _, _, vpcErr := sessionNetwork([]target{
		{service: "cloudsql", instanceRef: "db", netProject: "p", network: "a"},
		{service: "redis", instanceRef: "r/c", netProject: "p", network: "b"},
	})
	_, _, refErr := parseNetworkRef("projects/p/networks/n", "p")
	_, _, noNetErr := pickNetwork("", "", "", "p", nil)
	_, _, manyNetErr := pickNetwork("", "", "", "p", []string{"a", "b"})
	_, _, pickErr := holdPorts([]int{15432}, []bool{true}, taken)
	_, _, noFreeErr := holdPorts([]int{15432}, []bool{false}, taken)
	_, durErr := validateDuration("10 minutes")
	_, rangeErr := validateDuration("1s")
	portCause, _ := tunnel.Explain([]string{"bind [127.0.0.1]:15432: Address already in use"})

	cases := []struct {
		name string
		err  error
	}{
		{"unsupported service", validate(connectConfig{Service: "mysql", Instance: "x"})},
		{"missing flags", validate(connectConfig{})},
		{"bad --duration", validate(connectConfig{Instance: "db", Duration: "10 minutes"})},
		{"--duration out of range", rangeErr},
		{"unparsable --duration", durErr},
		{"bad --image", validate(connectConfig{Instance: "db", Image: "nope"})},
		{"-p out of range", validateValues(connectConfig{Port: 70000})},
		{"--network not a network", validateValues(connectConfig{Network: "projects/p/networks/n"})},
		{"-p 0", checkPortZero(map[string]bool{"p": true}, 0)},
		{"stack member", stackMember(two, 1, usage(validate(connectConfig{Service: "mysql"})))},
		{"stack on two VPCs", vpcErr},
		{"bad --network", refErr},
		{"no network", noNetErr},
		{"many networks", manyNetErr},
		{"fixed port taken", pickErr},
		{"no free port", noFreeErr},
		{"bastion status unknown", ended(errors.New("get vm: boom"), false, "", false, now, nil, nil)},
		{"bastion status unknown, with a fix", ended(gcperr.Wrap("get vm", &googleapi.Error{Code: 403, Message: "denied"}), false, "", false, now, nil, nil)},
		{"tunnel refused by IAP", ended(nil, true, "RUNNING", false, now, []string{"error 4033"}, nil)},
		{"tunnel kept dropping", ended(nil, true, "RUNNING", false, now, []string{"ssh: oops"}, nil)},
		{"tunnel kept dropping, port taken", ended(nil, true, "RUNNING", false, now, nil, errors.New(portCause))},
		{"session expired", ended(nil, false, "", false, later, nil, nil)},
		{"spot preempted", ended(nil, true, "TERMINATED", true, now, nil, nil)},
		{"bastion stopped", ended(nil, true, "TERMINATED", false, now, nil, nil)},
		{"cleanup failed", errors.New(cleanupMessage("p", "bqto-x", false, true, gcperr.Wrap("delete", &googleapi.Error{Code: 403, Message: "denied"})))},
		{"API 403", gcperr.Wrap("list", &googleapi.Error{Code: 403, Message: "denied"})},
		{"API 404", gcperr.Wrap("get", &googleapi.Error{Code: 404, Message: "not found"})},
		{"API 429", gcperr.Wrap("list", &googleapi.Error{Code: 429, Message: "slow down"})},
		{"API 500", gcperr.Wrap("list", &googleapi.Error{Code: 500, Message: "Internal error."})},
		{"API unreachable", gcperr.Wrap("list", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})},
		{"bad flag", cmdConnect([]string{"-bogus"})},
		{"a Pro service", validate(connectConfig{Service: "kafka", Instance: "r/x"})},
		{"a Pro flag", cmdConnect([]string{"--project=p", "--instance=i", "--no-input", "--spot"})},
		{"a profile name", cmdConnect([]string{"db", "--no-input"})},
		{"a Pro command", run([]string{"stack", "list"})},
	}
	for _, c := range cases {
		checkOneFix(t, c.name, c.err)
	}
}
