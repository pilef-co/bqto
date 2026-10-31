package cli

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/alloydb"
	"github.com/pilef-co/bqto/internal/cloudsql"
	"github.com/pilef-co/bqto/internal/redis"
	"github.com/pilef-co/bqto/internal/tunnel"
	"github.com/pilef-co/bqto/internal/valkey"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s changed; if that is intended, run go test -run %s -update\n--- got\n%s\n--- want\n%s", path, t.Name(), got, want)
	}
}

func TestUsageGolden(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	printUsage()
	os.Stdout = stdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	golden(t, "usage", string(out))
}

func TestProBinaryUsageGolden(t *testing.T) {
	out, errOut, code := runMain(t, "main-pro", "-h")
	if code != 0 || errOut != "" {
		t.Fatalf("bqto Pro -h: exit %d, stderr %q", code, errOut)
	}
	golden(t, "usage-pro", out)
}

func TestConnectHelpGolden(t *testing.T) {
	var cfg ext.Config
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	registerConnectFlags(fs, &cfg)
	var out bytes.Buffer
	printFlags(&out, fs)
	golden(t, "connect-help", out.String())
}

func goldenTarget(service string, spec ext.Target) target {
	t := target{service: service, instanceRef: spec.Instance, spec: spec}
	if spec.Forward != nil {
		t.fwd = tunnel.Forward{RemoteHost: spec.Forward.Host, RemotePort: spec.Forward.Port}
	}
	return t
}

func TestReadyLinesGolden(t *testing.T) {
	targets := []target{
		goldenTarget("cloudsql", cloudsql.Target(cloudsql.Endpoint{IP: "10.10.0.3", Port: 5432}, "sql", "", "")),
		goldenTarget("alloydb", alloydb.Target(alloydb.Endpoint{IP: "10.0.0.4", Port: 5432}, "us-central1/c/primary", "", "")),
		goldenTarget("redis", redis.Target(redis.Endpoint{IP: "10.0.0.3", Port: 6378, Region: "us-central1", TLS: true, Auth: true, ReadIP: "10.0.0.4", ReadPort: 6379}, "us-central1/cache", "", "")),
		goldenTarget("valkey", valkey.Target(valkey.Endpoint{IP: "10.0.0.5", Port: 6379, Region: "us-central1"}, "us-central1/valkey", "", "")),
		socksTarget("us-central1/thing"),
	}
	targets[4].started = ext.Started{Ports: map[string]int{"thing_port": 19092}, After: []string{"thing: started"}}

	for _, inStack := range []bool{false, true} {
		var out strings.Builder
		for i, tg := range targets {
			ready := ext.Ready{Index: i, LocalPort: 16000 + i, Started: tg.started}
			for _, line := range readyLines(tg, ready, "bqto-12345678", "expires at 14:28 (in 29m)", inStack) {
				out.WriteString(line + "\n")
			}
		}
		name := "ready-single"
		if inStack {
			name = "ready-stack"
		}
		got := strings.ReplaceAll(out.String(), "valkey-cli", "redis-cli")
		golden(t, name, got)
	}
}
