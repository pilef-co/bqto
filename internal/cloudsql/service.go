package cloudsql

import (
	"context"
	"fmt"
	"strconv"

	"github.com/pilef-co/bqto/ext"
)

func init() {
	ext.RegisterService(ext.Service{
		Name:          "cloudsql",
		Title:         "Cloud SQL",
		Noun:          "instances",
		InstanceTitle: "Instance",
		RefHelp:       "instance name (cloudsql)",
		Order:         10,
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
		Env:            PostgresEnv,
		Hints: func(s ext.Ready) []string {
			if h := clientHint(ep.Port, s.LocalPort); h != "" {
				return []string{h}
			}
			return nil
		},
	}
}

func clientHint(remotePort, localPort int) string {
	switch remotePort {
	case 5432:
		return fmt.Sprintf("try: psql \"host=127.0.0.1 port=%d user=postgres\"", localPort)
	case 3306:
		return fmt.Sprintf("try: mysql -h 127.0.0.1 -P %d -u root -p", localPort)
	case 1433:
		return fmt.Sprintf("try: sqlcmd -S 127.0.0.1,%d -U sqlserver -C", localPort)
	}
	return ""
}

func PostgresEnv(localPort int) []string {
	return []string{"PGHOST=127.0.0.1", "PGPORT=" + strconv.Itoa(localPort)}
}
