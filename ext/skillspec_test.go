package ext

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestTheAgentSkillFollowsTheAgentSkillsSpec(t *testing.T) {
	dir := filepath.Join("..", "skills", "bqto")
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body, ok := strings.CutPrefix(string(b), "---\n")
	if !ok {
		t.Fatal("skills/bqto/SKILL.md does not start with a --- frontmatter line")
	}
	head, _, ok := strings.Cut(body, "\n---\n")
	if !ok {
		t.Fatal("skills/bqto/SKILL.md has no closing --- after its frontmatter")
	}
	allowed := []string{"name", "description", "license", "compatibility", "metadata", "allowed-tools"}
	fields := map[string]string{}
	for line := range strings.SplitSeq(head, "\n") {
		if strings.HasPrefix(line, " ") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found || !slices.Contains(allowed, key) {
			t.Errorf("frontmatter line %q is not one of the fields %v", line, allowed)
			continue
		}
		value = strings.TrimSpace(value)
		if strings.Contains(value, ": ") || strings.Contains(value, " #") {
			t.Errorf("frontmatter %s holds %q, which YAML would not read as one plain string; quote it", key, value)
		}
		fields[key] = value
	}
	name := fields["name"]
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(name) || len(name) > 64 {
		t.Errorf("name %q must be 1 to 64 of a-z, 0-9 and single hyphens", name)
	}
	if name != filepath.Base(dir) {
		t.Errorf("name %q must equal its folder, %s", name, filepath.Base(dir))
	}
	if d := fields["description"]; d == "" || len(d) > 1024 || !strings.Contains(d, "Use when") {
		t.Errorf("description must be 1 to 1024 characters and say when to use the skill, got %d: %q", len(d), d)
	}
}
