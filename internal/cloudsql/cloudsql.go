package cloudsql

import (
	"github.com/pilef-co/bqto/gcperr"

	"context"
	"fmt"
	"strings"

	"github.com/pilef-co/bqto/quota"
	"google.golang.org/api/option"
	"google.golang.org/api/sqladmin/v1"
)

var clientOptions []option.ClientOption

type Endpoint struct {
	IP             string
	Port           int
	Region         string
	NetworkProject string
	Network        string
}

type Choice struct {
	Value string
	Label string
}

func Choices(ctx context.Context, project string) ([]Choice, error) {
	svc, err := sqladmin.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return nil, gcperr.Wrap("connect to the Cloud SQL Admin API", err)
	}
	resp, err := gcperr.RetryDo(ctx, svc.Instances.List(project).Context(ctx).Do)
	if err != nil {
		return nil, gcperr.Wrap("list Cloud SQL instances in project "+project, err)
	}
	choices := make([]Choice, 0, len(resp.Items))
	for _, inst := range resp.Items {
		choices = append(choices, choiceFor(inst))
	}
	return choices, nil
}

func choiceFor(inst *sqladmin.DatabaseInstance) Choice {
	c := Choice{Value: inst.Name, Label: inst.Name}
	if inst.InstanceType == "READ_REPLICA_INSTANCE" {
		primary := inst.MasterInstanceName
		if i := strings.LastIndex(primary, ":"); i >= 0 {
			primary = primary[i+1:]
		}
		c.Label = fmt.Sprintf("%s (read replica of %s)", inst.Name, primary)
	}
	return c
}

func Resolve(ctx context.Context, project, instance string) (Endpoint, error) {
	svc, err := sqladmin.NewService(ctx, quota.Options(ctx, clientOptions...)...)
	if err != nil {
		return Endpoint{}, gcperr.Wrap("connect to the Cloud SQL Admin API", err)
	}
	inst, err := gcperr.RetryDo(ctx, svc.Instances.Get(project, instance).Context(ctx).Do)
	if err != nil {
		return Endpoint{}, gcperr.Get("Cloud SQL instance", instance, project, ListCmd(project), err)
	}
	ep, err := endpointFrom(inst)
	if err != nil {
		return Endpoint{}, err
	}
	return ep, nil
}

func endpointFrom(inst *sqladmin.DatabaseInstance) (Endpoint, error) {
	if err := usable(inst); err != nil {
		return Endpoint{}, err
	}
	port, err := portFor(inst.DatabaseVersion)
	if err != nil {
		return Endpoint{}, err
	}

	var ipc *sqladmin.IpConfiguration
	if inst.Settings != nil {
		ipc = inst.Settings.IpConfiguration
	}

	if ipc != nil && ipc.PscConfig != nil {
		for _, ac := range ipc.PscConfig.PscAutoConnections {
			if ac.IpAddress == "" {
				continue
			}
			np, net, parseErr := parseNetwork(ac.ConsumerNetwork)
			if parseErr != nil {
				return Endpoint{}, fmt.Errorf("parse psc consumer network %q: %w", ac.ConsumerNetwork, parseErr)
			}
			return Endpoint{
				IP:             ac.IpAddress,
				Port:           port,
				Region:         inst.Region,
				NetworkProject: np,
				Network:        net,
			}, nil
		}
	}

	for _, m := range inst.IpAddresses {
		if m.Type != "PRIVATE" {
			continue
		}
		privNet := ""
		if ipc != nil {
			privNet = ipc.PrivateNetwork
		}
		np, net, parseErr := parseNetwork(privNet)
		if parseErr != nil {
			return Endpoint{}, fmt.Errorf("parse private network %q: %w", privNet, parseErr)
		}
		return Endpoint{
			IP:             m.IpAddress,
			Port:           port,
			Region:         inst.Region,
			NetworkProject: np,
			Network:        net,
		}, nil
	}

	return Endpoint{}, fmt.Errorf(
		"instance %q has no private endpoint (public-only or manual PSC)\n"+
			"Fix: give the instance a private IP, or let Private Service Connect create an endpoint in your VPC",
		inst.Name,
	)
}

func usable(inst *sqladmin.DatabaseInstance) error {
	switch inst.State {
	case "", "RUNNABLE", "ONLINE_MAINTENANCE":
	case "PENDING_CREATE", "MAINTENANCE", "REPAIRING":
		return fmt.Errorf("instance %q is not ready yet (state %s)\nFix: wait until it is RUNNABLE and run bqto again", inst.Name, inst.State)
	default:
		return fmt.Errorf("instance %q is not running (state %s)\nFix: choose an instance that is RUNNABLE", inst.Name, inst.State)
	}
	if inst.Settings != nil && inst.Settings.ActivationPolicy == "NEVER" {
		return fmt.Errorf("instance %q is stopped\nFix: start it with gcloud sql instances patch %s --activation-policy=ALWAYS --project %s", inst.Name, inst.Name, inst.Project)
	}
	return nil
}

func portFor(dbVersion string) (int, error) {
	switch {
	case strings.HasPrefix(dbVersion, "POSTGRES_"):
		return 5432, nil
	case strings.HasPrefix(dbVersion, "MYSQL_"):
		return 3306, nil
	case strings.HasPrefix(dbVersion, "SQLSERVER_"):
		return 1433, nil
	}
	return 0, fmt.Errorf("unknown database version %q; cannot determine port\nFix: this is a bqto bug; report it with this message", dbVersion)
}

func parseNetwork(path string) (project, network string, err error) {

	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[0] != "projects" || parts[2] != "global" || parts[3] != "networks" || parts[1] == "" || parts[4] == "" {
		return "", "", fmt.Errorf("unexpected network path format (want projects/P/global/networks/N)")
	}
	return parts[1], parts[4], nil
}

func ListCmd(project string) string {
	return "gcloud sql instances list --project " + project
}
