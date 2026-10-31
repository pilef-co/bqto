package cli

import (
	"slices"
	"strings"
	"testing"
)

func FuzzParseNetworkRef(f *testing.F) {
	for _, seed := range []string{"default", "projects/p/global/networks/vpc", "projects//global/networks/", "projects/p/global/networks/", "a/b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		project, network, err := parseNetworkRef(ref, "default-project")
		if err != nil || !strings.Contains(ref, "/") {
			return
		}
		if project == "" || network == "" || strings.Contains(project+network, "/") {
			t.Errorf("parseNetworkRef(%q) = %q, %q: a path must give a project and a network", ref, project, network)
		}
	})
}

func FuzzSplitConnectArgs(f *testing.F) {
	f.Add("prod-db --project=p -- psql -h 127.0.0.1")
	f.Add("--service redis")
	f.Add("-- --")
	f.Fuzz(func(t *testing.T, line string) {
		args := strings.Fields(line)
		name, flags, cmd := splitConnectArgs(args)
		var back []string
		if name != "" {
			back = append(back, name)
		}
		back = append(back, flags...)
		if slices.Contains(args, "--") {
			back = append(append(back, "--"), cmd...)
		}
		if !slices.Equal(back, args) {
			t.Errorf("splitConnectArgs(%q) = %q, %q, %q, which does not add up to the arguments", args, name, flags, cmd)
		}
	})
}
