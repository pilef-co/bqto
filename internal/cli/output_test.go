package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/cloudsql"
	"github.com/pilef-co/bqto/internal/redis"
	"github.com/pilef-co/bqto/internal/valkey"
)

func TestReadyEvents(t *testing.T) {
	deadline := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	thing := socksTarget("us-central1/thing")
	thing.started = ext.Started{JSON: map[string]any{"thing_port": 19094}}
	targets := []target{
		goldenTarget("cloudsql", cloudsql.Target(cloudsql.Endpoint{IP: "10.0.0.5", Port: 5432}, "sql", "", "")),
		thing,
		goldenTarget("redis", redis.Target(redis.Endpoint{IP: "10.0.0.7", Port: 6379, TLS: true, Auth: true}, "us-central1/cache", "", "")),
		goldenTarget("valkey", valkey.Target(valkey.Endpoint{IP: "10.0.0.8", Port: 6379, IAMAuth: true}, "us-central1/kv", "", "")),
	}
	got := readyEvents(targets, []int{15432, 1080, 16379, 16380}, "bqto-1", deadline)
	want := []string{
		`{"event":"ready","service":"cloudsql","instance":"sql","host":"127.0.0.1","port":15432,"remote":"10.0.0.5:5432","bastion":"bqto-1","expires":"2026-10-14T12:30:00Z"}`,
		`{"event":"ready","service":"fakesocks","instance":"us-central1/thing","host":"127.0.0.1","port":1080,"proxy":"socks5","remote":"thing.example:9092","auth":"sasl","thing_port":19094,"url":"https://thing.example","bastion":"bqto-1","expires":"2026-10-14T12:30:00Z"}`,
		`{"event":"ready","service":"redis","instance":"us-central1/cache","host":"127.0.0.1","port":16379,"remote":"10.0.0.7:6379","tls":true,"auth":"password","bastion":"bqto-1","expires":"2026-10-14T12:30:00Z"}`,
		`{"event":"ready","service":"valkey","instance":"us-central1/kv","host":"127.0.0.1","port":16380,"remote":"10.0.0.8:6379","auth":"iam","bastion":"bqto-1","expires":"2026-10-14T12:30:00Z"}`,
	}
	for i := range want {
		line, err := json.Marshal(got[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(line) != want[i] {
			t.Errorf("event %d = %s\nwant      %s", i, line, want[i])
		}
	}
}

func TestOutputFormatIsChecked(t *testing.T) {
	err := cmdConnect([]string{"--project=p", "--instance=i", "--no-input", "--output=yaml"})
	if err == nil || !strings.Contains(err.Error(), `--output "yaml" is not a format`) {
		t.Errorf("err = %v, want the format error", err)
	}
	err = cmdConnect([]string{"--project=p", "--instance=i", "--no-input", "--output=json", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "--output json and -- CMD both write to stdout") {
		t.Errorf("err = %v, want --output json refused with -- CMD", err)
	}
}

func TestSessionWritesOnlyTheReadyEventToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = stdout })

	s := testSession()
	s.cfg.Output = "json"
	cloud := &fakeCloud{clock: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runSession(ctx, s, cloud.deps()) }()

	lines := bufio.NewReader(r)
	read := make(chan string, 1)
	go func() {
		line, _ := lines.ReadString('\n')
		read <- line
	}()
	var first string
	select {
	case first = <-read:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("no ready event on stdout while the session was open")
	}
	select {
	case err := <-done:
		t.Fatalf("the session ended before it was stopped: %v", err)
	default:
	}
	var ev readyEvent
	if err := json.Unmarshal([]byte(first), &ev); err != nil {
		t.Fatalf("stdout line %q is not a ready event: %v", first, err)
	}
	if ev.Event != "ready" || ev.Port != 15432 || ev.Remote != "10.0.0.3:5432" || ev.Bastion != "bqto-12345678" {
		t.Errorf("event = %+v", ev)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("session after cancel: %v", err)
	}
	_ = w.Close()
	rest, _ := io.ReadAll(lines)
	if len(rest) != 0 {
		t.Errorf("stdout after the ready event: %q, want nothing", rest)
	}
}
