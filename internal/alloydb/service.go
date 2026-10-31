package alloydb

import (
	"context"
	"fmt"
	"strconv"

	"github.com/pilef-co/bqto/ext"
)

func init() {
	ext.RegisterService(ext.Service{
		Name:          "alloydb",
		Title:         "AlloyDB",
		Noun:          "instances",
		InstanceTitle: "Instance",
		RefHelp:       "region/cluster/instance (alloydb)",
		Order:         20,
		List:          listChoices,
		Resolve:       resolveTarget,
	})
}

func listChoices(ctx context.Context, project string) ([]ext.Choice, error) {
	choices, err := Choices(ctx, project)
	out := make([]ext.Choice, len(choices))
	for i, c := range choices {
		out[i] = ext.Choice{Label: c.Label, Value: c.Value}
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
	return ext.Target{
		Instance:       instance,
		NetworkProject: netProject,
		Network:        network,
		Region:         ep.Region,
		Forward:        &ext.Forward{Host: ep.IP, Port: ep.Port},
		Env: func(localPort int) []string {
			return []string{"PGHOST=127.0.0.1", "PGPORT=" + strconv.Itoa(localPort)}
		},
		Hints: func(s ext.Ready) []string {
			return []string{PsqlHint(s.LocalPort)}
		},
	}
}

func PsqlHint(port int) string {
	return fmt.Sprintf("try: psql \"host=127.0.0.1 port=%d user=postgres sslmode=require\"", port)
}
