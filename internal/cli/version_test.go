package cli

import (
	"context"
	"errors"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
	"github.com/pilef-co/bqto/internal/selfupdate"
)

func fakeUpdater(r ext.Release, err error) ext.Updater {
	return ext.Updater{
		Name:    "bqto",
		Line:    func() string { return "bqto v0.11.0" },
		Current: func() string { return "v0.11.0" },
		Latest:  func(context.Context) (ext.Release, error) { return r, err },
	}
}

func TestVersionCommand(t *testing.T) {
	newer := ext.Release{Version: "0.12.0", Newer: true}
	tests := []struct {
		name                string
		args                []string
		u                   ext.Updater
		debug               bool
		wantOut, wantErrOut string
	}{
		{"newer", nil, fakeUpdater(newer, nil), false, "bqto v0.11.0\n", "bqto 0.12.0 is available (you have 0.11.0).\nFix: bqto update\n"},
		{"security", nil, fakeUpdater(ext.Release{Version: "0.12.0", Newer: true, Security: true}, nil), false, "bqto v0.11.0\n", "bqto 0.12.0 fixes a security issue in your version.\nFix: bqto update\n"},
		{"up to date", nil, fakeUpdater(ext.Release{Version: "0.11.0"}, nil), false, "bqto v0.11.0\n", ""},
		{"check fails", nil, fakeUpdater(ext.Release{}, errors.New("GitHub answered 404\nFix: x")), false, "bqto v0.11.0\n", ""},
		{"check fails with --debug", nil, fakeUpdater(ext.Release{}, errors.New("GitHub answered 404\nFix: x")), true, "bqto v0.11.0\n", "debug: could not check for updates: GitHub answered 404\n"},
		{"json", []string{"--output", "json"}, fakeUpdater(newer, nil), false, `{"version":"0.11.0","latest":"0.12.0","update_available":true,"security":false}` + "\n", ""},
		{"json, check fails", []string{"--output=json"}, fakeUpdater(ext.Release{}, errors.New("offline")), false, `{"version":"0.11.0","latest":"","update_available":false,"security":false}` + "\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			if tc.debug {
				ext.SetDebug(&errOut)
				t.Cleanup(func() { ext.SetDebug(nil) })
			}
			if err := cmdVersion(tc.args, tc.u, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.wantOut || errOut.String() != tc.wantErrOut {
				t.Errorf("stdout %q stderr %q; want %q and %q", out.String(), errOut.String(), tc.wantOut, tc.wantErrOut)
			}
		})
	}
}

func TestVersionCommandRefusesBadArgs(t *testing.T) {
	for _, args := range [][]string{{"--output", "yaml"}, {"extra"}, {"--nope"}} {
		err := cmdVersion(args, fakeUpdater(ext.Release{}, nil), &strings.Builder{}, &strings.Builder{})
		if code := report(&strings.Builder{}, err); code != exitUsage || !strings.Contains(err.Error(), "\nFix: ") {
			t.Errorf("%v: err %v, exit %d", args, err, code)
		}
	}
}

func TestDefaultUpdaterIsTheFreeOne(t *testing.T) {
	u, custom := activeUpdater()
	if custom || u.Name != "bqto" || u.Timeout != 2e9 || u.Line() != "bqto "+buildVersion() || strings.Contains(u.DirFix, "sudo bqto update") {
		t.Errorf("activeUpdater() = %+v, custom %v", u, custom)
	}
}

func TestUpdateHandsOverToBrewOrGoOnlyInTheFreeBinary(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	brew := "/opt/homebrew/Cellar/bqto/0.11.0/bin/bqto"
	version = "v0.11.0"
	if got := selfupdate.Detect(installOf(brew, nil, false)); got != selfupdate.Homebrew {
		t.Errorf("free binary in the Cellar: %v, want Homebrew", got)
	}
	if got := selfupdate.Detect(installOf(brew, nil, true)); got != selfupdate.Binary {
		t.Errorf("custom updater in the Cellar: %v, want Binary", got)
	}
	in := installOf("/x/bqto", &debug.BuildInfo{Path: selfupdate.MainPackage, Main: debug.Module{Version: "v0.11.0"}}, false)
	if !in.Stamped {
		t.Error("a release build with -X version is not marked stamped")
	}
	version = "dev"
	if installOf("/x/bqto", nil, false).Stamped {
		t.Error("a dev build is marked stamped")
	}
}
