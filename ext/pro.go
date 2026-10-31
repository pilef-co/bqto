package ext

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

var (
	ProAvailable = true
	ProJoin      = "join the bqto Pro beta at https://bqto.dev/#waitlist"
)

func ProTag() string {
	if ProAvailable {
		return "(bqto Pro)"
	}
	return "(not available)"
}

var (
	proServices = map[string]string{
		"kafka":    "Managed Kafka",
		"cloudrun": "Cloud Run",
	}
	proVariants = map[string]string{
		"redis-cluster":  "Memorystore for Redis Cluster",
		"valkey-cluster": "Memorystore for Valkey in cluster mode",
	}
	proCommands = map[string]proCommand{
		"profile": {label: "profiles", usage: "bqto profile COMMAND", about: "saved connections"},
		"stack":   {label: "stacks", usage: "bqto stack COMMAND", about: "groups of profiles"},
	}
)

func ProService(name string) (string, bool) {
	label, ok := proServices[name]
	return label, ok
}

func ProVariant(name string) (string, bool) {
	label, ok := proVariants[name]
	return label, ok
}

type proCommand struct {
	label string
	usage string
	about string
}

func ProCommand(name string) (string, bool) {
	c, ok := proCommands[name]
	return c.label, ok
}

func ProUsage() []string {
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(proCommands)) {
		if _, registered := LookupCommand(name); registered {
			continue
		}
		c := proCommands[name]
		lines = append(lines, fmt.Sprintf("%s  %s %s", c.usage, c.about, ProTag()))
	}
	return lines
}

var proExamples = [][2]string{
	{"bqto connect --project P --service cloudsql --instance orders --save db", "connect, and save the session as the profile db"},
	{"bqto profile create cache --project P --service redis --instance cache", "save a profile without connecting"},
	{"bqto connect db", "open a profile by name"},
	{"bqto stack create work --profile db --profile cache", "group profiles into a stack"},
	{"bqto connect --stack work", "open every profile of the stack through one bastion"},
}

func ProExamples() string {
	return formatExamples(proExamples)
}

func formatExamples(examples [][2]string) string {
	blocks := make([]string, 0, len(examples))
	for _, e := range examples {
		blocks = append(blocks, fmt.Sprintf("  %s\n      %s\n", e[0], e[1]))
	}
	return strings.Join(blocks, "\n")
}

func ProHelp() string {
	var b strings.Builder
	if ProAvailable {
		b.WriteString("bqto Pro: saved connections (profiles) and groups of them (stacks).\n")
	} else {
		b.WriteString("Saved connections (profiles) and groups of them (stacks).\n")
	}
	b.WriteString(ProExamples() + "\n")
	b.WriteString(formatExamples([][2]string{{"bqto profile list | show NAME | rename OLD NEW | rm NAME", "(the same for stack)"}}))
	if ProAvailable {
		b.WriteString("\n" + proNeeded() + "\n")
	} else {
		b.WriteString("\nThese commands are not available in this build.\n")
	}
	return b.String()
}

func proNeeded() string {
	line := "These commands need bqto Pro: " + ProJoin + "."
	if len(line) > 80 {
		return "These commands need bqto Pro:\n" + ProJoin + "."
	}
	return line
}

type ProFlag struct {
	Name   string
	Bool   bool
	Target bool
	Usage  string
}

var proFlags = []ProFlag{
	{Name: "profile", Usage: "open a saved profile, or several separated by commas"},
	{Name: "stack", Usage: "open every profile of a saved stack in one session"},
	{Name: "save", Usage: "save this session as a profile"},
	{Name: "replace", Bool: true, Usage: "with --save, overwrite the profile already saved under that name"},
	{Name: "web", Bool: true, Target: true, Usage: "start a local HTTP reverse proxy that adds your identity token, cloudrun only"},
	{Name: "spot", Bool: true, Target: true, Usage: "use a Spot VM, which costs less and may be preempted"},
	{Name: "subnet", Target: true, Usage: "subnet self-link, or a short name that expands to the full self-link"},
	{Name: "machine-type", Target: true, Usage: "bastion VM machine type, default e2-micro"},
	{Name: "image", Target: true, Usage: "bastion image, projects/P/global/images/IMAGE or projects/P/global/images/family/FAMILY"},
	{Name: "endpoint", Target: true, Usage: "connect to a private IP:PORT instead of a named instance"},
}

func ProFlags() []ProFlag {
	return append([]ProFlag(nil), proFlags...)
}

type ProError struct {
	What string
	Drop string
}

func (e ProError) Error() string {
	if ProAvailable {
		return fmt.Sprintf("bqto Pro is needed for %s\nFix: %s, or %s", e.What, e.Drop, ProJoin)
	}
	verb := "is"
	if isProCommandLabel(e.What) {
		verb = "are"
	}
	return fmt.Sprintf("%s %s not available in this build\nFix: %s", e.What, verb, e.Drop)
}

func isProCommandLabel(what string) bool {
	for _, c := range proCommands {
		if c.label == what {
			return true
		}
	}
	return false
}

type UsageError struct {
	Err error
}

func (e UsageError) Error() string { return e.Err.Error() }

func (e UsageError) Unwrap() error { return e.Err }

func Usagef(format string, a ...any) error {
	return UsageError{Err: fmt.Errorf(format, a...)}
}
