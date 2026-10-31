package redis

import (
	"strings"
	"testing"
)

func FuzzParseNetwork(f *testing.F) {
	for _, seed := range []string{"projects/p/global/networks/vpc", "projects//global/networks/", "projects/p/global/networks/", "vpc", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		project, network, err := parseNetwork(path)
		if err != nil || !strings.Contains(path, "/") {
			return
		}
		if project == "" || network == "" || strings.Contains(project+network, "/") {
			t.Errorf("parseNetwork(%q) = %q, %q: a path must give a project and a network", path, project, network)
		}
	})
}
