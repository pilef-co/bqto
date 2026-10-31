package bastion

import (
	"strings"
	"testing"
)

func FuzzParseImageRef(f *testing.F) {
	for _, seed := range []string{DefaultImage, "projects/p/global/images/img", "projects//global/images/family/f", "https://www.googleapis.com/compute/v1/projects/p/global/images/img"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		project, family, image, err := parseImageRef(ref)
		if err != nil {
			return
		}
		if project == "" || (family == "") == (image == "") || strings.Contains(project+family+image, "/") {
			t.Errorf("parseImageRef(%q) = %q, %q, %q: want a project and exactly one of family or image", ref, project, family, image)
		}
	})
}

func FuzzParseSubnetPath(f *testing.F) {
	for _, seed := range []string{"projects/p/regions/r/subnetworks/s", "projects//regions//subnetworks/", "https://www.googleapis.com/compute/v1/projects/p/regions/r/subnetworks/s"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, subnet string) {
		project, region, name, err := parseSubnetPath(subnet)
		if err != nil {
			return
		}
		if project == "" || region == "" || name == "" || strings.Contains(project+region+name, "/") {
			t.Errorf("parseSubnetPath(%q) = %q, %q, %q: want three non-empty segments", subnet, project, region, name)
		}
	})
}
