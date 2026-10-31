package cli

import (
	"flag"
	"fmt"
	"io"

	"slices"
	"strings"

	"github.com/pilef-co/bqto/ext"
)

var shells = []string{"bash", "zsh", "fish"}

const bashCompletion = `_bqto() {
	local typed=("${COMP_WORDS[@]:1:COMP_CWORD}") line
	COMPREPLY=()
	while IFS= read -r line; do
		COMPREPLY+=("$line")
	done < <(bqto __complete "${typed[@]}" 2>/dev/null)
}
complete -F _bqto bqto
`

const zshCompletion = `#compdef bqto
_bqto() {
	local -a candidates
	candidates=(${(f)"$(bqto __complete "${words[@]:1:$((CURRENT-1))}" 2>/dev/null)"})
	compset -P '-*='
	compadd -a candidates
}
if [ "$funcstack[1]" = "_bqto" ]; then
	_bqto "$@"
else
	compdef _bqto bqto
fi
`

const fishCompletion = `function __bqto_complete
	set -l cur (commandline -ct)
	set -l prefix (string match -r -- '^-[^=]*=' "$cur")
	for candidate in (bqto __complete (commandline -opc)[2..-1] "$cur" 2>/dev/null)
		echo "$prefix$candidate"
	end
end
complete -c bqto -f -a '(__bqto_complete)'
`

func cmdCompletion(args []string) error {
	if len(args) != 1 || !slices.Contains(shells, args[0]) {
		return usagef("bqto completion needs one shell: %s\nFix: add to your shell's startup file: source <(bqto completion zsh), or eval \"$(bqto completion bash)\", or bqto completion fish | source", strings.Join(shells, ", "))
	}
	script := map[string]string{"bash": bashCompletion, "zsh": zshCompletion, "fish": fishCompletion}[args[0]]
	fmt.Print(script)
	return nil
}

func cmdComplete(args []string) error {
	for _, c := range completions(args) {
		fmt.Println(c)
	}
	return nil
}

func completions(args []string) []string {
	debugGiven := false
	for len(args) > 1 && isDebugFlag(args[0]) {
		debugGiven = true
		if len(args) > 3 && args[1] == "=" {
			args = args[3:]
			continue
		}
		args = args[1:]
	}
	if len(args) == 0 {
		args = []string{""}
	}
	cur := args[len(args)-1]
	if len(args) == 1 {
		if strings.HasPrefix(cur, "-") {
			if debugGiven {
				return nil
			}
			return withPrefix([]string{"--debug"}, cur)
		}
		return withPrefix(commandNames(), cur)
	}
	switch args[0] {
	case "connect":
		before := args[1 : len(args)-1]
		if name, value, ok := strings.Cut(cur, "="); ok && strings.HasPrefix(name, "-") {
			before = append(slices.Clone(before), name)
			cur = value
		}
		return withPrefix(connectCompletions(before, cur), cur)
	case "completion":
		if len(args) == 2 {
			return withPrefix(shells, cur)
		}
	case "help", "--help", "-help", "-h":
		if len(args) == 2 {
			return withPrefix(commandNames(), cur)
		}
	default:
		if c, ok := ext.LookupCommand(args[0]); ok && c.Complete != nil {
			return withPrefix(c.Complete(args[1:]), cur)
		}
	}
	return nil
}

func isDebugFlag(arg string) bool {
	name, _, _ := strings.Cut(arg, "=")
	return name == "--debug" || name == "-debug"
}

func commandNames() []string {
	names := []string{"connect"}
	for _, c := range sortedCommands() {
		names = append(names, c.Name)
	}
	return append(names, "completion", "version", "update", "help")
}

func connectCompletions(before []string, cur string) []string {
	if slices.Contains(before, "--") {
		return nil
	}
	if n := len(before); n > 0 && before[n-1] == "=" {
		before = before[:n-1]
	}
	if n := len(before); n > 0 && before[n-1] == ":" {
		return nil
	}
	prev := ""
	if len(before) > 0 {
		prev = strings.TrimLeft(before[len(before)-1], "-")
	}
	switch prev {
	case "service":
		var names []string
		for _, s := range ext.Services() {
			names = append(names, s.Name)
		}
		return names
	case "output":
		return []string{"text", "json"}
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerConnectFlags(fs, &ext.Config{})
	if prev != "" && strings.HasPrefix(before[len(before)-1], "-") && !strings.Contains(prev, "=") {
		if f := fs.Lookup(prev); f != nil && !isBoolFlag(f) {
			return nil
		}
	}
	if cur != "" && !strings.HasPrefix(cur, "-") {
		return nil
	}
	var names []string
	fs.VisitAll(func(f *flag.Flag) {
		if _, placeholder := f.Value.(proPlaceholder); placeholder {
			return
		}
		if len(f.Name) == 1 {
			names = append(names, "-"+f.Name)
			return
		}
		names = append(names, "--"+f.Name)
	})
	return names
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func withPrefix(candidates []string, prefix string) []string {
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
