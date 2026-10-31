package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/ext"
)

func TestSplitConnectArgs(t *testing.T) {
	tests := []struct {
		in          []string
		wantProfile string
		wantFlags   []string
		wantCmd     []string
	}{
		{
			in:          []string{"prod-db", "--project=foo", "--", "psql", "-h", "127.0.0.1"},
			wantProfile: "prod-db",
			wantFlags:   []string{"--project=foo"},
			wantCmd:     []string{"psql", "-h", "127.0.0.1"},
		},
		{
			in:          []string{"--project=foo", "--instance=bar"},
			wantProfile: "",
			wantFlags:   []string{"--project=foo", "--instance=bar"},
			wantCmd:     nil,
		},
		{
			in:          []string{"--", "psql"},
			wantProfile: "",
			wantFlags:   []string{},
			wantCmd:     []string{"psql"},
		},
		{
			in:          []string{"my-profile"},
			wantProfile: "my-profile",
			wantFlags:   []string{},
			wantCmd:     nil,
		},
		{
			in:          nil,
			wantProfile: "",
			wantFlags:   nil,
			wantCmd:     nil,
		},
		{
			in:          []string{"my-profile", "--spot", "--", "redis-cli"},
			wantProfile: "my-profile",
			wantFlags:   []string{"--spot"},
			wantCmd:     []string{"redis-cli"},
		},
	}

	for _, tc := range tests {
		gotProfile, gotFlags, gotCmd := splitConnectArgs(tc.in)
		if gotProfile != tc.wantProfile {
			t.Errorf("splitConnectArgs(%v) profile = %q, want %q", tc.in, gotProfile, tc.wantProfile)
		}
		if !sliceEq(gotFlags, tc.wantFlags) {
			t.Errorf("splitConnectArgs(%v) flags = %v, want %v", tc.in, gotFlags, tc.wantFlags)
		}
		if !sliceEq(gotCmd, tc.wantCmd) {
			t.Errorf("splitConnectArgs(%v) cmd = %v, want %v", tc.in, gotCmd, tc.wantCmd)
		}
	}
}

func TestMissingFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  connectConfig
		want []string
	}{
		{
			name: "all present",
			cfg: connectConfig{
				Project:  "p",
				Service:  "cloudsql",
				Instance: "i",
			},
			want: nil,
		},
		{
			name: "missing instance and service",
			cfg:  connectConfig{Project: "p"},
			want: []string{"--service", "--instance"},
		},
		{
			name: "missing project",
			cfg:  connectConfig{Service: "cloudsql", Instance: "i"},
			want: []string{"--project"},
		},
		{
			name: "everything empty",
			cfg:  connectConfig{},
			want: []string{"--project", "--service", "--instance"},
		},
	}

	for _, tc := range tests {
		got := missingFields(tc.cfg)
		if !sliceEq(got, tc.want) {
			t.Errorf("%s: missingFields = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLeftoverArgs(t *testing.T) {
	err := cmdConnect([]string{"--project=foo", "--instance=bar", "extra"})
	if err == nil {
		t.Fatal("expected error for leftover args, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected arguments") {
		t.Errorf("error = %q, want 'unexpected arguments'", err.Error())
	}
}

func TestMergeWizardResult_ExplicitDuration(t *testing.T) {
	tests := []struct {
		name         string
		cfgDuration  string
		wizDuration  string
		wantDuration string
	}{
		{
			name:         "explicit 30m not overridden by wizard 1h",
			cfgDuration:  "30m",
			wizDuration:  "1h",
			wantDuration: "30m",
		},
		{
			name:         "empty duration filled from wizard",
			cfgDuration:  "",
			wizDuration:  "2h",
			wantDuration: "2h",
		},
		{
			name:         "explicit 1h not overridden by wizard 2h",
			cfgDuration:  "1h",
			wizDuration:  "2h",
			wantDuration: "1h",
		},
	}
	for _, tc := range tests {
		cfg := connectConfig{Duration: tc.cfgDuration}
		w := connectConfig{Duration: tc.wizDuration}
		mergeWizardResult(&cfg, w)
		if cfg.Duration != tc.wantDuration {
			t.Errorf("%s: duration = %q, want %q", tc.name, cfg.Duration, tc.wantDuration)
		}
	}
}

func TestAProServiceIsRefusedBeforeAnything(t *testing.T) {
	for svc, label := range map[string]string{"cloudrun": "Cloud Run", "kafka": "Managed Kafka"} {
		err := cmdConnect([]string{"--project=p", "--service=" + svc, "--instance=r/x", "--no-input"})
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != exitUsage || err.Error() != "bqto Pro is needed for "+label+"\nFix: use another --service, or join the bqto Pro beta at https://bqto.dev/#waitlist" {
			t.Errorf("--service %s: err = %v, want exit 2 and the Pro error", svc, err)
		}
	}
}

func TestValidateDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{in: "30m", want: 30 * time.Minute},
		{in: "30s", want: 30 * time.Second},
		{in: "2880h", want: 2880 * time.Hour},
		{in: "29s", wantErr: "--duration 29s is out of range"},
		{in: "0", wantErr: "--duration 0 is out of range"},
		{in: "-5m", wantErr: "--duration -5m is out of range"},
		{in: "2881h", wantErr: "--duration 2881h is out of range"},
		{in: "10 minutes", wantErr: "--duration: time: unknown unit"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := validateDuration(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("validateDuration(%q) = %v, %v; want %v, nil", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestDurationRejectedBeforeNetwork(t *testing.T) {
	err := cmdConnect([]string{"--project=p", "--instance=i", "--duration=10s", "--no-input"})
	if err == nil || !strings.Contains(err.Error(), "--duration 10s is out of range") {
		t.Errorf("err = %v, want the --duration range error", err)
	}
}

func TestImageRejectedBeforeNetwork(t *testing.T) {
	_, err := prepareConfigs([]connectConfig{{Project: "p", Instance: "i", Image: "debian-13"}})
	if err == nil || !strings.Contains(err.Error(), `cannot parse image "debian-13"`) {
		t.Errorf("err = %v, want the image parse error", err)
	}
}

func TestProFlagsAreRefusedInTheFreeBinary(t *testing.T) {
	for _, flagArgs := range [][]string{{"--web"}, {"--spot"}, {"--image", "x"}, {"--endpoint", "10.0.0.5:5432"}, {"--save", "db"}, {"--profile", "db"}, {"--stack", "s"}} {
		args := append([]string{"--project=p", "--instance=i", "--no-input"}, flagArgs...)
		err := cmdConnect(args)
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != exitUsage || err.Error() != "bqto Pro is needed for "+flagArgs[0]+"\nFix: drop "+flagArgs[0]+", or join the bqto Pro beta at https://bqto.dev/#waitlist" {
			t.Errorf("connect %v: err = %v, want exit 2 and the Pro error", flagArgs, err)
		}
	}
}

func TestAProfileNameIsRefusedInTheFreeBinary(t *testing.T) {
	err := cmdConnect([]string{"db", "--no-input"})
	if err == nil || !strings.HasPrefix(err.Error(), "bqto Pro is needed for profiles\nFix: give --service and --instance instead of") {
		t.Errorf("err = %v, want the Pro error for a profile name", err)
	}
}

func TestAClusterFoundByLookupIsRefused(t *testing.T) {
	_, err := resolveVariant(context.Background(), ext.Host{}, ext.Request{}, ext.Target{}, ext.NeedsFeature{Feature: "redis-cluster", Ref: "r/x", Name: "projects/p/locations/r/clusters/x"})
	var ec exitCodeError
	if !errors.As(err, &ec) || ec.code != exitUsage || err.Error() != "bqto Pro is needed for Memorystore for Redis Cluster (r/x)\nFix: connect to an instance instead, or join the bqto Pro beta at https://bqto.dev/#waitlist" {
		t.Errorf("err = %v, want exit 2 and the bqto Pro refusal", err)
	}
}

func TestHiddenProTheFreeBinarySaysNotAvailable(t *testing.T) {
	before := ext.ProAvailable
	t.Cleanup(func() { ext.ProAvailable = before })
	ext.ProAvailable = false
	err := cmdConnect([]string{"--project=p", "--instance=i", "--no-input", "--spot"})
	var ec exitCodeError
	if !errors.As(err, &ec) || ec.code != exitUsage || err.Error() != "--spot is not available in this build\nFix: drop --spot" {
		t.Errorf("err = %v, want exit 2 and the hidden-mode wording", err)
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	registerConnectFlags(fs, &ext.Config{})
	var out bytes.Buffer
	printFlags(&out, fs)
	if !strings.Contains(out.String(), "use a Spot VM, which costs less and may be preempted (not available)\n") || strings.Contains(out.String(), "bqto Pro") {
		t.Errorf("help does not mark the Pro flags with (not available):\n%s", out.String())
	}
}

func TestPortsRejectedBeforeNetwork(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"-p", "70000"}, want: "-p 70000 is not a port\nFix: use a port from 1 to 65535"},
		{args: []string{"-p", "-5"}, want: "-p -5 is not a port\nFix: use a port from 1 to 65535"},
		{args: []string{"--network", "projects/p/networks/n"}, want: "is not a network"},
		{args: []string{"-p", "0"}, want: "-p 0 is not a port"},
	}
	for _, tc := range tests {
		args := append([]string{"--project=p", "--instance=i", "--no-input"}, tc.args...)
		err := cmdConnect(args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("connect %v: err = %v, want it to contain %q", tc.args, err, tc.want)
		}
		var ec exitCodeError
		if !errors.As(err, &ec) || ec.code != exitUsage {
			t.Errorf("connect %v: err = %v, want exit code %d", tc.args, err, exitUsage)
		}
	}
}

func TestOutputJSONNeverPrompts(t *testing.T) {
	cases := []struct {
		desc     string
		cfg      ext.Config
		terminal bool
		want     bool
	}{
		{"a terminal with text output may prompt", ext.Config{Output: "text"}, true, false},
		{"--output json on a terminal never prompts", ext.Config{Output: "json"}, true, true},
		{"--no-input never prompts", ext.Config{NoInput: true}, true, true},
		{"no terminal never prompts", ext.Config{}, false, true},
		{"--end-on-stdin-close never prompts", ext.Config{EndOnStdinClose: true}, true, true},
	}
	for _, c := range cases {
		if got := noInput(c.cfg, c.terminal); got != c.want {
			t.Errorf("%s: noInput = %v, want %v", c.desc, got, c.want)
		}
	}
}
