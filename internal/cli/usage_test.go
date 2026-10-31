package cli

import (
	"strings"
	"testing"
)

func TestAnUndescribedLongUsageLineDoesNotWidenTheColumn(t *testing.T) {
	long := parseUsageLine("bqto profile create NAME --project P --service S --instance I [flags]")
	groups := append(usageGroups(), []usageLine{long})
	for l := range strings.SplitSeq(formatUsage(groups), "\n") {
		if strings.Contains(strings.TrimSpace(l), "  ") && len(l) > 80 {
			t.Errorf("described line %q is %d columns, want at most 80", l, len(l))
		}
	}
}

func TestTheProHelperUsageLinesSplitIntoCommandAndDescription(t *testing.T) {
	for name, lines := range proHelperUsage {
		for _, s := range lines {
			parts := strings.Split(s, "  ")
			l := parseUsageLine(s)
			if len(parts) != 2 || l.about == "" || strings.Contains(l.command, "  ") {
				t.Errorf("%s: %q splits into %q, want one double space between a command and a description", name, s, parts)
			}
		}
	}
}

func TestABareConnectUsageLineJoinsTheConnectGroup(t *testing.T) {
	l := parseUsageLine("bqto connect  open the wizard, then connect")
	if l.command != "bqto connect" || !isConnectUsage(l) {
		t.Errorf("parseUsageLine gave %+v, want the command bqto connect in the connect group", l)
	}
}
