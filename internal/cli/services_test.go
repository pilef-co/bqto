package cli

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/quota"

	_ "github.com/pilef-co/bqto/internal/alloydb"
	_ "github.com/pilef-co/bqto/internal/cloudsql"
	_ "github.com/pilef-co/bqto/internal/redis"
	_ "github.com/pilef-co/bqto/internal/valkey"
)

const fakeSOCKS = "fakesocks"

var quotaProjects sync.Map

func init() {
	ext.RegisterService(ext.Service{
		Name:          fakeSOCKS,
		Title:         "Fake SOCKS service",
		Noun:          "things",
		InstanceTitle: "Thing",
		RefHelp:       "region/name (fakesocks)",
		Order:         99,
		Resolve: func(ctx context.Context, _ ext.Host, r ext.Request) (ext.Target, error) {
			quotaProjects.Store(r.Instance, quota.Project(ctx))
			return fakeSOCKSTarget(r.Instance), nil
		},
	})
}

func fakeSOCKSTarget(instance string) ext.Target {
	return ext.Target{
		Instance:       instance,
		NetworkProject: "p",
		Network:        "default",
		Region:         "us-central1",
		Summary:        "thing " + instance + " (thing.example)",
		Remote:         "thing.example:9092",
		Auth:           "sasl",
		JSON:           map[string]any{"url": "https://thing.example"},
		Env: func(port int) []string {
			return []string{"ALL_PROXY=socks5h://127.0.0.1:" + strconv.Itoa(port)}
		},
		Start: func(_ context.Context, s ext.Ready) (ext.Started, error) {
			port, release, err := s.ReservePort(19092 + s.Index)
			if err != nil {
				return ext.Started{}, fmt.Errorf("find thing port: %w", err)
			}
			return ext.Started{
				Ports: map[string]int{"thing_port": port},
				Env:   []string{"BQTO_THING_PORT=" + strconv.Itoa(port)},
				JSON:  map[string]any{"thing_port": port},
				After: []string{"thing: started"},
				Close: release,
			}, nil
		},
		Hints: func(s ext.Ready) []string {
			return []string{fmt.Sprintf("try: thing --proxy 127.0.0.1:%d --port %d", s.LocalPort, s.Started.Ports["thing_port"])}
		},
	}
}

func socksTarget(instance string) target {
	spec := fakeSOCKSTarget(instance)
	return target{service: fakeSOCKS, instanceRef: instance, netProject: spec.NetworkProject, network: spec.Network, region: spec.Region, spec: spec}
}
