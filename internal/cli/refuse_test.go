package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
)

func onATerminalWithoutPrompts(t *testing.T) *bool {
	t.Helper()
	terminal, runWiz := stdinIsTerminal, wizard
	t.Cleanup(func() { stdinIsTerminal, wizard = terminal, runWiz })
	prompted := new(bool)
	stdinIsTerminal = func() bool { return true }
	wizard = func(context.Context, connectConfig) (connectConfig, error) {
		*prompted = true
		return connectConfig{}, errors.New("the wizard ran")
	}
	return prompted
}

func TestTheRefusalsComeBeforeAnyPrompt(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--service", "kafka"}, "bqto Pro is needed for Managed Kafka\nFix: use another --service, or join the bqto Pro beta at https://bqto.dev/#waitlist"},
		{[]string{"--service", "cloudrun", "--project", "p"}, "bqto Pro is needed for Cloud Run\nFix: use another --service, or join the bqto Pro beta at https://bqto.dev/#waitlist"},
		{[]string{"--service", "mysql"}, "--service \"mysql\" is not supported\nFix: use "},
		{[]string{"--endpoint", "10.0.0.5:5432"}, "bqto Pro is needed for --endpoint\nFix: drop --endpoint, or join the bqto Pro beta at https://bqto.dev/#waitlist"},
		{[]string{"--service", "cloudsql", "--spot"}, "bqto Pro is needed for --spot\nFix: drop --spot, or join the bqto Pro beta at https://bqto.dev/#waitlist"},
		{[]string{"db"}, "bqto Pro is needed for profiles\nFix: give --service and --instance instead of \"db\", or join the bqto Pro beta at https://bqto.dev/#waitlist"},
	} {
		prompted := onATerminalWithoutPrompts(t)
		err := cmdConnect(tc.args)
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != exitUsage || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("connect %v: err = %v, want exit 2 and %q", tc.args, err, tc.want)
		}
		if *prompted {
			t.Errorf("connect %v: the wizard ran before the refusal", tc.args)
		}
	}
}

func TestEndOnStdinCloseRefusesATerminalAndACommandWithoutPrompting(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--project", "p", "--instance", "sql", "--end-on-stdin-close"}, "--end-on-stdin-close needs a pipe on stdin, and stdin is a terminal\nFix: "},
		{[]string{"--project", "p", "--service", "redis", "--end-on-stdin-close"}, "missing required flags: --instance\nFix: "},
		{[]string{"--project", "p", "--instance", "sql", "--end-on-stdin-close", "--", "psql"}, "--end-on-stdin-close and -- CMD both use stdin\nFix: "},
	} {
		prompted := onATerminalWithoutPrompts(t)
		err := cmdConnect(tc.args)
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != exitUsage || !strings.HasPrefix(ec.err.Error(), tc.want) {
			t.Errorf("connect %v: err = %v, want exit 2 and %q", tc.args, err, tc.want)
		}
		if *prompted {
			t.Errorf("connect %v: the wizard read stdin", tc.args)
		}
	}
}

func TestAKnownServiceStillOpensTheWizard(t *testing.T) {
	prompted := onATerminalWithoutPrompts(t)
	if err := cmdConnect([]string{"--service", "redis"}); err == nil || !*prompted {
		t.Errorf("connect --service redis on a terminal: err = %v, prompted = %v, want the wizard", err, *prompted)
	}
}

const resolveHookOption = "bqto-test-resolve-hook"

func init() {
	ext.RegisterConnect(ext.ConnectHooks{
		Resolve: func(_ context.Context, h ext.Host, c ext.Config) (ext.Target, bool, error) {
			switch c.Option(resolveHookOption) {
			case "":
				return ext.Target{}, false, nil
			case "fail":
				return ext.Target{}, false, errors.New("the hook failed")
			}
			netProject, network, err := h.NetworkFor(c.Network, c.Project, "", "")
			return ext.Target{Instance: c.Instance, NetworkProject: netProject, Network: network, Region: c.Region, Forward: &ext.Forward{Host: "10.9.0.5", Port: 6000}}, true, err
		},
	})
}

func TestAResolveHookTakesTheTargetBeforeTheService(t *testing.T) {
	cfg := connectConfig{Project: "p", Service: "no-such-service", Instance: "i", Network: "vpc", Region: "r", Options: map[string]string{resolveHookOption: "on", "kept": "yes"}}
	got, err := resolveTarget(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if got.service != "no-such-service" || got.instanceRef != "i" || got.netProject != "p" || got.network != "vpc" || got.region != "r" ||
		got.fwd.RemoteHost != "10.9.0.5" || got.fwd.RemotePort != 6000 || got.options["kept"] != "yes" {
		t.Errorf("target = %+v, want the hook's target with the session's options", got)
	}

	cfg.Options[resolveHookOption] = "fail"
	if _, err := resolveTarget(context.Background(), cfg, nil); err == nil || err.Error() != "the hook failed" {
		t.Errorf("a failing hook: err = %v, want its error", err)
	}

	delete(cfg.Options, resolveHookOption)
	if _, err := resolveTarget(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), `--service "no-such-service" is not supported`) {
		t.Errorf("an unhandled config: err = %v, want the service lookup", err)
	}
}

func TestTheCoreValidateRefusesAPaidFlagNoHookRegistered(t *testing.T) {
	for _, f := range ext.ProFlags() {
		name := f.Name
		err := (coreAPI{}).Validate(ext.Config{Project: "p", Service: "cloudsql", Instance: "i"}, map[string]bool{name: true})
		if want := "bqto Pro is needed for --" + name + "\nFix: drop --" + name + ", or join the bqto Pro beta at https://bqto.dev/#waitlist"; err == nil || err.Error() != want {
			t.Errorf("Validate with --%s: err = %v, want %q", name, err, want)
		}
	}
	free := ext.Config{Project: "p", Service: "cloudsql", Instance: "i"}
	if err := (coreAPI{}).Validate(free, map[string]bool{"project": true}); err != nil {
		t.Errorf("Validate with free flags only: %v", err)
	}
}
