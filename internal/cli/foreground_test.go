package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/pilef-co/bqto/ext"
)

func TestHelperProcess(_ *testing.T) {
	switch os.Getenv("BQTO_TEST_HELPER") {
	case "foreground":
		fmt.Printf("foreground=%v\n", terminalForeground())
	case "brokenpipe":
		surviveBrokenPipes()
		for range 3 {
			fmt.Println("deleting bastion…")
		}
		if err := os.WriteFile(os.Getenv("BQTO_TEST_MARKER"), nil, 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "complete", "main":
		os.Exit(Main(os.Args[slices.Index(os.Args, "--")+1:]))
	case "main-hidden":
		ext.ProAvailable = false
		os.Exit(Main(os.Args[slices.Index(os.Args, "--")+1:]))
	case "main-pro":
		for _, name := range []string{"profile", "stack"} {
			ext.RegisterCommand(ext.Command{Name: name, Usage: proHelperUsage[name], Run: func(args []string) error {
				fmt.Printf("%s's own help %v\n", name, args)
				return nil
			}})
		}
		ext.RegisterConnect(ext.ConnectHooks{
			Flags: func(fs *flag.FlagSet, _ *ext.Config) {
				for _, f := range ext.ProFlags() {
					if !f.Target {
						fs.String(f.Name, "", f.Usage)
					}
				}
			},
			TargetFlags: func(fs *flag.FlagSet, _ *ext.Config) {
				for _, f := range ext.ProFlags() {
					if f.Target {
						fs.String(f.Name, "", f.Usage)
					}
				}
			},
		})
		os.Exit(Main(os.Args[slices.Index(os.Args, "--")+1:]))
	}
}

var proHelperUsage = map[string][]string{
	"profile": {
		"bqto  pick a profile, or start the wizard",
		"bqto connect PROFILE[,PROFILE...]  open saved profiles",
		"bqto profile create|list|show|rename|rm  save and manage profiles",
	},
	"stack": {
		"bqto connect --stack NAME  open every profile of a stack",
		"bqto stack create|list|show|rename|rm  save and manage stacks",
	},
}

func helper(t *testing.T, kind string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	c.Env = append(os.Environ(), "BQTO_TEST_HELPER="+kind)
	return c
}

func TestBrokenStdoutDoesNotKillTheCleanup(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	defer w.Close()
	marker := filepath.Join(t.TempDir(), "deleted")
	c := helper(t, "brokenpipe")
	c.Env = append(c.Env, "BQTO_TEST_MARKER="+marker)
	c.Stdout = w
	if err := c.Run(); err != nil {
		t.Errorf("writing to a closed pipe: %v, want bqto to carry on", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the step after the broken write never ran")
	}
}

func TestForegroundAsksTheTerminalNotStdin(t *testing.T) {
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is not installed")
	}
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", `'\''`) + "'"
	inner := quoted + " -test.run='^TestHelperProcess$' </dev/null"
	var c *exec.Cmd
	if runtime.GOOS == "darwin" {
		c = exec.Command(script, "-q", "/dev/null", "sh", "-c", inner)
	} else {
		c = exec.Command(script, "-qec", inner, "/dev/null")
	}
	c.Env = append(os.Environ(), "BQTO_TEST_HELPER=foreground", "TERM=dumb")
	out, err := c.CombinedOutput()
	if !strings.Contains(string(out), "foreground=") {
		t.Skipf("script could not give the helper a terminal: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "foreground=true") {
		t.Errorf("in the terminal's foreground with stdin from /dev/null: %s, want foreground=true", out)
	}
}
