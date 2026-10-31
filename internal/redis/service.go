package redis

import (
	"context"
	"net"
	"strconv"

	"github.com/pilef-co/bqto/ext"
)

func init() {
	ext.RegisterService(ext.Service{
		Name:          "redis",
		Title:         "Memorystore for Redis",
		Noun:          "instances",
		InstanceTitle: "Instance",
		RefHelp:       "region/instance (redis)",
		Order:         30,
		List:          listChoices,
		Resolve:       resolveTarget,
	})
}

func listChoices(ctx context.Context, project string) ([]ext.Choice, error) {
	refs, err := List(ctx, project)
	out := make([]ext.Choice, len(refs))
	for i, r := range refs {
		out[i] = ext.Choice{Label: r, Value: r}
	}
	return out, err
}

func resolveTarget(ctx context.Context, h ext.Host, r ext.Request) (ext.Target, error) {
	h.Progress("resolving endpoint…")
	ep, err := Resolve(ctx, r.Project, r.Instance)
	if err != nil {
		return ext.Target{}, err
	}
	netProject, network, err := h.NetworkFor(r.Network, r.Project, ep.NetworkProject, ep.Network)
	if err != nil {
		return ext.Target{}, err
	}
	return Target(ep, r.Instance, netProject, network), nil
}

func Target(ep Endpoint, instance, netProject, network string) ext.Target {
	auth := ""
	if ep.Auth {
		auth = "password"
	}
	return ext.Target{
		Instance:       instance,
		NetworkProject: netProject,
		Network:        network,
		Region:         ep.Region,
		Forward:        &ext.Forward{Host: ep.IP, Port: ep.Port},
		Remote:         net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port)),
		TLS:            ep.TLS,
		Auth:           auth,
		Hints: func(s ext.Ready) []string {
			return []string{Hint(ep, instance, s.LocalPort)}
		},
	}
}
