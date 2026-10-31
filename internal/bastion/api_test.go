package bastion

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pilef-co/bqto/internal/gcpfake"
)

func insertPath(zone string) string {
	return "/compute/v1/projects/p/zones/" + zone + "/instances"
}

func doneOp(zone string) map[string]any {
	return map[string]any{"name": "op-" + zone, "zone": "zones/" + zone, "status": "DONE"}
}

func failedOp(zone, code, message string) map[string]any {
	op := doneOp(zone)
	op["httpErrorStatusCode"] = 503
	op["httpErrorMessage"] = "SERVICE UNAVAILABLE"
	op["error"] = map[string]any{"errors": []map[string]any{{"code": code, "message": message}}}
	return op
}

func opPath(zone string) string {
	return "/compute/v1/projects/p/zones/" + zone + "/operations/op-" + zone
}

func runningOp(zone string) map[string]any {
	op := doneOp(zone)
	op["status"] = "RUNNING"
	return op
}

func stockout(fake *gcpfake.Server, zone string) *gcpfake.Server {
	return fake.
		OK("POST", insertPath(zone), runningOp(zone)).
		OK("GET", opPath(zone), failedOp(zone, "ZONE_RESOURCE_POOL_EXHAUSTED", "out of e2-micro"))
}

func testSpec() Spec {
	return Spec{Project: "p", Name: "bqto-1234abcd", Subnet: "projects/p/regions/us-central1/subnetworks/default", Duration: time.Hour, Image: DefaultImage}
}

func TestCreateInZonesAgainstTheAPI(t *testing.T) {
	zones := []string{"us-central1-a", "us-central1-b"}

	t.Run("a stockout moves to the next zone", func(t *testing.T) {
		fake := stockout(gcpfake.Start(t, &clientOptions), "us-central1-a").
			OK("POST", insertPath("us-central1-b"), runningOp("us-central1-b")).
			OK("GET", opPath("us-central1-b"), doneOp("us-central1-b"))
		var retried []string
		zone, err := CreateInZones(context.Background(), testSpec(), zones, func(z, next string) { retried = append(retried, z+"->"+next) })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if zone != "us-central1-b" || !slices.Equal(retried, []string{"us-central1-a->us-central1-b"}) {
			t.Errorf("zone = %s, retried = %v", zone, retried)
		}
		if n := len(fake.Requests()); n != 4 {
			t.Errorf("%d requests, want 4", n)
		}
	})

	t.Run("a policy violation stops at the first zone", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("POST", insertPath("us-central1-a"), http.StatusBadRequest, "conditionNotMet",
			"Constraint constraints/compute.trustedImageProjects violated for project p.")
		_, err := CreateInZones(context.Background(), testSpec(), zones, func(string, string) { t.Error("retried after a policy error") })
		var pe *policyError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), "projects/debian-cloud in constraints/compute.trustedImageProjects") {
			t.Errorf("err = %v, want the policy and its fix", err)
		}
	})

	t.Run("every zone out of capacity", func(t *testing.T) {
		fake := gcpfake.Start(t, &clientOptions)
		stockout(fake, "us-central1-a")
		stockout(fake, "us-central1-b")
		_, err := CreateInZones(context.Background(), testSpec(), zones, func(string, string) {})
		if err == nil || !strings.Contains(err.Error(), "zones us-central1-a, us-central1-b are out of capacity") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestDeleteAgainstTheAPI(t *testing.T) {
	const path = "/compute/v1/projects/p/zones/us-central1-a/instances/bqto-1234abcd"

	t.Run("a VM that is already gone is not an error", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).Err("DELETE", path, http.StatusNotFound, "notFound", "not found")
		requested, err := Delete(context.Background(), "p", "us-central1-a", "bqto-1234abcd", false)
		if err != nil || requested {
			t.Errorf("Delete = %v, %v; want false, nil", requested, err)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("DELETE", path, doneOp("us-central1-a"))
		requested, err := Delete(context.Background(), "p", "us-central1-a", "bqto-1234abcd", false)
		if err != nil || !requested {
			t.Errorf("Delete = %v, %v; want true, nil", requested, err)
		}
	})
}

func TestCheckFirewallAgainstTheAPI(t *testing.T) {
	const (
		networkPath  = "/compute/v1/projects/p/global/networks/vpc"
		regionalPath = "/compute/v1/projects/p/regions/us-central1/firewallPolicies/getEffectiveFirewalls"
		globalPath   = "/compute/v1/projects/p/global/networks/vpc/getEffectiveFirewalls"
		networkURL   = "https://www.googleapis.com/compute/v1/projects/p/global/networks/vpc"
	)
	iapRule := func(name, action string, priority int, tags ...string) map[string]any {
		r := map[string]any{
			"name": name, "network": networkURL, "direction": "INGRESS", "priority": priority,
			"sourceRanges": []string{"35.235.240.0/20"}, "targetTags": tags,
		}
		r[action] = []map[string]any{{"IPProtocol": "tcp", "ports": []string{"22"}}}
		return r
	}

	t.Run("the allow rule gives the bastion its tags", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", networkPath, map[string]any{"name": "vpc"}).
			OK("GET", regionalPath+"?network=projects/p/global/networks/vpc", map[string]any{"firewalls": []any{iapRule("allow-iap", "allowed", 1000, "iap")}})
		tags, warnings, err := CheckFirewall(context.Background(), "p", "vpc", "us-central1")
		if err != nil || !slices.Equal(tags, []string{"iap"}) || len(warnings) != 0 {
			t.Errorf("CheckFirewall = %v, %v, %v", tags, warnings, err)
		}
	})

	t.Run("a deny with a lower number wins over the allow", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", networkPath, map[string]any{"name": "vpc"}).
			OK("GET", regionalPath, map[string]any{"firewalls": []any{
				iapRule("allow-iap", "allowed", 1000, "iap"),
				iapRule("deny-ssh", "denied", 900),
			}})
		_, _, err := CheckFirewall(context.Background(), "p", "vpc", "us-central1")
		if err == nil || !strings.Contains(err.Error(), `"deny-ssh" (priority 900) blocks tcp:22`) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("without the regional permission it checks the global rules and says so", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", networkPath, map[string]any{"name": "vpc"}).
			Err("GET", regionalPath, http.StatusForbidden, "forbidden", "Required 'compute.networks.getRegionEffectiveFirewalls' permission").
			OK("GET", globalPath, map[string]any{"firewalls": []any{iapRule("allow-iap", "allowed", 1000)}})
		_, warnings, err := CheckFirewall(context.Background(), "p", "vpc", "us-central1")
		if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "regional network firewall policies were not checked") {
			t.Errorf("CheckFirewall = %v, %v", warnings, err)
		}
	})

	t.Run("no rule lets IAP in", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).
			OK("GET", networkPath, map[string]any{"name": "vpc"}).
			OK("GET", regionalPath, map[string]any{})
		_, _, err := CheckFirewall(context.Background(), "p", "vpc", "us-central1")
		if err == nil || !strings.Contains(err.Error(), "gcloud compute firewall-rules create allow-iap-ssh") {
			t.Errorf("err = %v, want the command that creates the rule", err)
		}
	})
}

func TestSubnetAgainstTheAPI(t *testing.T) {
	const path = "/compute/v1/projects/p/regions/us-central1/subnetworks"
	sub := func(name, network, purpose string) map[string]any {
		return map[string]any{
			"name": name, "purpose": purpose,
			"network":  "https://www.googleapis.com/compute/v1/projects/p/global/networks/" + network,
			"selfLink": "https://www.googleapis.com/compute/v1/projects/p/regions/us-central1/subnetworks/" + name,
		}
	}

	t.Run("skips subnets that cannot host a VM and other networks", func(t *testing.T) {
		gcpfake.Start(t, &clientOptions).OK("GET", path, map[string]any{"items": []any{
			sub("a-proxy", "vpc", "REGIONAL_MANAGED_PROXY"),
			sub("b-other", "other", "PRIVATE"),
			sub("c-vms", "vpc", "PRIVATE"),
		}})
		got, err := Subnet(context.Background(), "p", "vpc", "us-central1")
		if err != nil || !strings.HasSuffix(got, "/subnetworks/c-vms") {
			t.Errorf("Subnet = %q, %v", got, err)
		}
	})

	for _, tc := range []struct {
		desc string
		pro  []string
		fix  string
	}{
		{"none can host a VM, free", nil, "\nFix: create a subnet in us-central1"},
		{"none can host a VM, pro", []string{"subnet"}, "\nFix: create a subnet in us-central1, or pass --subnet"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			withProFlags(t, tc.pro...)
			gcpfake.Start(t, &clientOptions).OK("GET", path, map[string]any{"items": []any{sub("a-proxy", "vpc", "REGIONAL_MANAGED_PROXY")}})
			_, err := Subnet(context.Background(), "p", "vpc", "us-central1")
			if err == nil || !strings.Contains(err.Error(), "proxy-only, PSC and Private NAT subnets are skipped") || !strings.HasSuffix(err.Error(), tc.fix) {
				t.Errorf("err = %v, want it to end with %q", err, tc.fix)
			}
		})
	}
}

func TestIDAgainstTheAPI(t *testing.T) {
	const path = "/compute/v1/projects/p/zones/us-central1-a/instances/bqto-1234abcd"
	gcpfake.Start(t, &clientOptions).OK("GET", path, map[string]any{"id": "4242", "name": "bqto-1234abcd"})
	id, err := ID(context.Background(), "p", "us-central1-a", "bqto-1234abcd")
	if err != nil || id != 4242 {
		t.Errorf("ID = %d, %v; want 4242", id, err)
	}
}
