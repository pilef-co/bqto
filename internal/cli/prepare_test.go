package cli

import (
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/gcperr"
)

func TestEveryStackMemberIsValidated(t *testing.T) {
	old := defaultProject
	defaultProject = func() string { return "p" }
	t.Cleanup(func() { defaultProject = old })

	cfgs := []connectConfig{
		{Project: "p", Service: "cloudsql", Instance: "sql"},
		{Project: "p", Service: "reddis", Instance: "us-central1/cache"},
	}
	_, err := prepareConfigs(cfgs)
	if err == nil || !strings.Contains(err.Error(), "--service") || !strings.Contains(err.Error(), "service 2 of 2") {
		t.Errorf("err = %v, want the service error naming the second profile", err)
	}
	if msg, fix := gcperr.SplitFix(err); !strings.HasPrefix(msg, "service 2 of 2 (") || fix == "" || strings.Contains(fix, "service 2 of 2") {
		t.Errorf("err = %q, want the profile first and the Fix line last", err)
	}

	cfgs = []connectConfig{
		{Project: "p", Service: "cloudsql", Instance: "sql"},
		{Project: "p", Service: "redis", Instance: "us-central1/cache", Port: 70000},
	}
	if _, err := prepareConfigs(cfgs); err == nil || !strings.Contains(err.Error(), "-p 70000") {
		t.Errorf("err = %v, want the port error of the second profile", err)
	}
}

func TestPrivateGoogleAccessForAnyMemberThatNeedsIt(t *testing.T) {
	db := target{service: "cloudsql"}
	needs := target{service: fakeSOCKS, spec: ext.Target{NeedsPrivateGoogleAccess: true}}
	if !needsPrivateGoogleAccess([]target{db, needs}) {
		t.Error("the member that needs it is second in a stack: Private Google Access is not checked")
	}
	if needsPrivateGoogleAccess([]target{db, {service: "redis"}}) {
		t.Error("no member needs it: Private Google Access is checked anyway")
	}
}

func TestPrepareFillsTheDefaultsAHookSaves(t *testing.T) {
	old := defaultProject
	defaultProject = func() string { return "from-gcloud" }
	t.Cleanup(func() { defaultProject = old })
	cfgs := []connectConfig{{Service: "redis", Instance: "us-central1/cache"}}
	if _, err := prepareConfigs(cfgs); err != nil {
		t.Fatal(err)
	}
	if cfgs[0].Project != "from-gcloud" || cfgs[0].Duration != "30m" {
		t.Errorf("prepared %+v, want the defaulted project and duration", cfgs[0])
	}
}
