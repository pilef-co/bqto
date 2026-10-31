package cli

import (
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/tunnel"
)

func TestReadyForward(t *testing.T) {
	cases := []struct {
		desc        string
		instanceRef string
		want        string
	}{
		{
			desc:        "a named instance",
			instanceRef: "bqto-test-sql",
			want:        "ready: 127.0.0.1:15432 → 10.0.0.5:5432 (instance bqto-test-sql), bastion bqto-abc, expires at 19:30 (in 6m)",
		},
		{
			desc:        "an instance named by its address is not repeated",
			instanceRef: "10.0.0.5:5432",
			want:        "ready: 127.0.0.1:15432 → 10.0.0.5:5432, bastion bqto-abc, expires at 19:30 (in 6m)",
		},
		{
			desc:        "no reference at all",
			instanceRef: "",
			want:        "ready: 127.0.0.1:15432 → 10.0.0.5:5432, bastion bqto-abc, expires at 19:30 (in 6m)",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := readyForward(15432, "10.0.0.5", 5432, c.instanceRef, "bqto-abc", "expires at 19:30 (in 6m)")
			if got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestReadyLinesInAStack(t *testing.T) {
	db := target{service: "cloudsql", instanceRef: "sql-main", fwd: tunnel.Forward{RemoteHost: "10.0.0.5", RemotePort: 5432}}
	lines := readyLines(db, ext.Ready{LocalPort: 15432}, "bqto-abc", "expires at 19:30 (in 6m)", true)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "  ") {
		t.Errorf("a stack line should be indented: %q", lines[0])
	}
	for _, want := range []string{"127.0.0.1:15432", "10.0.0.5:5432", "cloudsql sql-main"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line is missing %q: %s", want, lines[0])
		}
	}
	if strings.Contains(lines[0], "bqto-abc") {
		t.Errorf("the bastion is named once for the whole stack, not per service: %s", lines[0])
	}

	alone := readyLines(db, ext.Ready{LocalPort: 15432}, "bqto-abc", "expires at 19:30 (in 6m)", false)
	if !strings.Contains(alone[0], "bastion bqto-abc") {
		t.Errorf("a single session still names the bastion: %s", alone[0])
	}
}

func TestReadyLinesOfASOCKSService(t *testing.T) {
	tg := socksTarget("us-central1/thing")
	tg.started = ext.Started{Ports: map[string]int{"thing_port": 19092}, After: []string{"thing: started"}}
	lines := readyLines(tg, ext.Ready{LocalPort: 1080, Started: tg.started}, "bqto-abc", "expires at 19:30 (in 6m)", false)
	want := []string{
		"ready: SOCKS5 proxy 127.0.0.1:1080 → thing us-central1/thing (thing.example), bastion bqto-abc, expires at 19:30 (in 6m)",
		"try: thing --proxy 127.0.0.1:1080 --port 19092",
		"thing: started",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
