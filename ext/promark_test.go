package ext

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", path))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

func serviceRows(t *testing.T) (pro, free string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, found := strings.Cut(string(b), "\n## Services\n")
	if !found {
		t.Fatal("README.md has no section Services")
	}
	table, _, _ = strings.Cut(table, "\n## ")
	var proRows, freeRows []string
	for line := range strings.SplitSeq(table, "\n") {
		cells := strings.Split(line, "|")
		if !strings.HasPrefix(line, "|") || len(cells) < 5 {
			continue
		}
		switch plan := strings.TrimSpace(cells[3]); {
		case strings.Contains(plan, "bqto Pro"):
			proRows = append(proRows, strings.TrimSpace(cells[1]))
		case plan == "Free":
			freeRows = append(freeRows, strings.TrimSpace(cells[1]))
		}
	}
	if len(proRows) == 0 || len(freeRows) == 0 {
		t.Fatal("README.md, section Services, has no Free or bqto Pro rows")
	}
	return strings.Join(proRows, "\n"), strings.Join(freeRows, "\n")
}

func readmeProSection(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(b), "\n## bqto Pro\n")
	if !found {
		t.Fatal("README.md has no section bqto Pro")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return strings.Join(strings.Fields(section), " ")
}

func skillProSentence(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "skills", "bqto", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for paragraph := range strings.SplitSeq(string(b), "\n\n") {
		if sentence, _, found := strings.Cut(paragraph, "need bqto Pro."); found {
			return strings.Join(strings.Fields(sentence), " ")
		}
	}
	t.Fatal("skills/bqto/SKILL.md has no sentence that ends in need bqto Pro.")
	return ""
}

func TestTheDocsUseTheProMarkOfTheBinary(t *testing.T) {
	restoreProAvailable(t)
	mark := ProTag()
	ProAvailable = !ProAvailable
	otherMark := ProTag()
	ProAvailable = !ProAvailable
	for _, doc := range []string{"README.md", "skills/bqto/SKILL.md"} {
		text := readDoc(t, doc)
		if !strings.Contains(text, "`"+mark+"`") {
			t.Errorf("%s does not name the mark `%s` that bqto help prints", doc, mark)
		}
		if strings.Contains(text, otherMark) {
			t.Errorf("%s names %s, but the free binary marks bqto Pro features %s", doc, otherMark, mark)
		}
	}
	if readme := readDoc(t, "README.md"); ProAvailable && !strings.Contains(readme, ProJoin) {
		t.Errorf("README.md does not say %q, the Fix of a refusal", ProJoin)
	}
}

func TestTheDocsListTheSameProFeaturesAsTheBinary(t *testing.T) {
	pro, free := serviceRows(t)
	section := readmeProSection(t)
	skill := skillProSentence(t)

	for _, f := range proFlags {
		flag := regexp.MustCompile(`(?:^|[^\w-])--` + regexp.QuoteMeta(f.Name) + `(?:[^\w-]|$)`)
		if !flag.MatchString(section) {
			t.Errorf("README.md, section bqto Pro, does not name --%s", f.Name)
		}
	}
	for name, c := range proCommands {
		command := "`bqto " + name
		if !strings.Contains(section, command) {
			t.Errorf("README.md, section bqto Pro, does not name bqto %s", name)
		}
		if !strings.Contains(skill, c.label) {
			t.Errorf("skills/bqto/SKILL.md does not say that %s need bqto Pro", c.label)
		}
	}
	for _, labels := range []map[string]string{proServices, proVariants} {
		for _, label := range labels {
			if !strings.Contains(pro, label) {
				t.Errorf("README.md, section Services, has no bqto Pro row for %s", label)
			}
			if strings.Contains(free, label) {
				t.Errorf("README.md, section Services, names %s in a Free row", label)
			}
			if !strings.Contains(section, label) {
				t.Errorf("README.md, section bqto Pro, does not name %s", label)
			}
		}
	}
	for name, label := range proServices {
		if !strings.Contains(skill, label) {
			t.Errorf("skills/bqto/SKILL.md does not say that %s (%s) needs bqto Pro", label, name)
		}
	}
}

func TestTheReadmeShowsTheProExamplesOfTheHelp(t *testing.T) {
	section := readmeProSection(t)
	for l := range strings.SplitSeq(ProExamples(), "\n") {
		if command := strings.TrimSpace(l); strings.HasPrefix(command, "bqto ") && !strings.Contains(section, command) {
			t.Errorf("README.md, section bqto Pro, does not show the example %q of bqto profile -h", command)
		}
	}
}
