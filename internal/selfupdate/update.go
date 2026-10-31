package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pilef-co/bqto/ext"
)

const (
	CheckTimeout  = 2 * time.Second
	UpdateTimeout = 5 * time.Minute
	MainPackage   = "github.com/pilef-co/bqto/cmd/bqto"
)

type Status struct {
	Version  string `json:"version"`
	Latest   string `json:"latest"`
	Newer    bool   `json:"update_available"`
	Security bool   `json:"security"`
}

func Check(ctx context.Context, u ext.Updater) (Status, error) {
	have := u.Current()
	st := Status{Version: shown(have)}
	if !Released(have) {
		return st, fmt.Errorf("%s is a development build, which has no release to compare with", have)
	}
	timeout := u.Timeout
	if timeout <= 0 {
		timeout = CheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := u.Latest(ctx)
	if err != nil {
		return st, err
	}
	st.Latest, st.Newer, st.Security = r.Version, r.Newer, r.Newer && r.Security
	return st, nil
}

func Released(v string) bool {
	_, ok := ext.ParseVersion(v)
	return ok && !strings.ContainsAny(v, "-+")
}

func shown(v string) string {
	if Released(v) {
		return ext.ShortVersion(v)
	}
	return strings.TrimPrefix(v, "v")
}

func Notice(w io.Writer, name string, st Status) {
	if !st.Newer {
		return
	}
	if st.Security {
		fmt.Fprintf(w, "%s %s fixes a security issue in your version.\n", name, st.Latest)
	} else {
		fmt.Fprintf(w, "%s %s is available (you have %s).\n", name, st.Latest, st.Version)
	}
	fmt.Fprintln(w, "Fix: bqto update")
}

type Method int

const (
	Binary Method = iota
	Homebrew
	GoInstall
)

type Install struct {
	Exe        string
	Build      *debug.BuildInfo
	Stamped    bool
	GoBinDirs  func() []string
	AllowTools bool
}

func Detect(in Install) Method {
	if !in.AllowTools {
		return Binary
	}
	if exe := filepath.ToSlash(in.Exe); strings.Contains(exe, "/Cellar/bqto/") || strings.Contains(exe, "/Caskroom/bqto/") {
		return Homebrew
	}
	if in.Stamped || in.Build == nil || in.Build.Path != MainPackage {
		return Binary
	}
	if v := in.Build.Main.Version; v == "" || v == "(devel)" {
		return Binary
	}
	dir := filepath.Dir(in.Exe)
	for _, d := range in.GoBinDirs() {
		if d != "" && sameDir(d, dir) {
			return GoInstall
		}
	}
	return Binary
}

func sameDir(a, b string) bool {
	return resolved(a) == resolved(b)
}

func resolved(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

func GoBinDirs() []string {
	if _, err := exec.LookPath("go"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var dirs []string
	if len(lines) > 0 && lines[0] != "" {
		dirs = append(dirs, lines[0])
	}
	if len(lines) > 1 {
		for p := range strings.SplitSeq(lines[1], string(os.PathListSeparator)) {
			if p != "" {
				dirs = append(dirs, filepath.Join(p, "bin"))
			}
		}
	}
	return dirs
}

func ToolCommand(m Method) []string {
	switch m {
	case Homebrew:
		return []string{"brew", "upgrade", "bqto"}
	case GoInstall:
		return []string{"go", "install", MainPackage + "@latest"}
	}
	return nil
}

type Runner func(ctx context.Context, name string, args ...string) error

func RunTool(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name and args come from ToolCommand or SudoCommand
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

type Sudo struct {
	Run      Runner
	Terminal bool
	Root     bool
	Found    bool
}

const sudoScript = `set -e
PATH=/usr/bin:/bin:/usr/sbin:/sbin:/run/current-system/sw/bin
export PATH
t=$(mktemp "$3/.bqto-update-XXXXXX")
trap 'rm -f "$t"' EXIT
trap 'exit 1' HUP INT TERM
install -o "$5" -g "$6" -m "$1" "$2" "$t"
mv -f "$t" "$4"`

func SudoCommand(staged, exe string, mode fs.FileMode, uid, gid uint32) []string {
	return []string{"sudo", "/bin/sh", "-c", sudoScript, "sh", fmt.Sprintf("%#o", mode.Perm()), staged, filepath.Dir(exe), exe, strconv.FormatUint(uint64(uid), 10), strconv.FormatUint(uint64(gid), 10)}
}

func ownerOf(exe string) (uid, gid uint32, err error) {
	fi, err := os.Stat(exe)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("bqto cannot read the owner of %s", exe)
	}
	return st.Uid, st.Gid, nil
}

type Options struct {
	Updater ext.Updater
	Method  Method
	Exe     string
	Out     io.Writer
	Err     io.Writer
	Run     Runner
	Sudo    Sudo
}

func Update(ctx context.Context, o Options) error {
	if argv := ToolCommand(o.Method); argv != nil {
		how := map[Method]string{Homebrew: "Homebrew", GoInstall: "go install"}[o.Method]
		line := strings.Join(argv, " ")
		fmt.Fprintf(o.Out, "bqto was installed with %s, so bqto update runs: %s\n", how, line)
		if err := o.Run(ctx, argv[0], argv[1:]...); err != nil {
			return fmt.Errorf("%s failed: %w\nFix: run %s yourself and read its output", line, err, line)
		}
		return nil
	}
	u := o.Updater
	have := u.Current()
	if !Released(have) {
		return fmt.Errorf("this bqto is a development build (%s), so bqto update does not replace it\nFix: build it again from your checkout, or install a release as the README shows under Build and install", have)
	}
	ctx, cancel := context.WithTimeout(ctx, UpdateTimeout)
	defer cancel()
	r, err := u.Latest(ctx)
	if err != nil {
		if errors.Is(err, errNoPublicRelease) {
			return fmt.Errorf("%w\nFix: download the release by hand, as the README shows under Build and install", err)
		}
		return withFix(err, "check your network and run bqto update again")
	}
	if !r.Newer {
		fmt.Fprintf(o.Out, "%s %s is the newest version, so there is nothing to update.\n", u.Name, shown(have))
		return nil
	}
	elevate, err := needsSudo(o)
	if err != nil {
		return withFix(err, u.DirFix)
	}
	dir, err := os.MkdirTemp("", "bqto-update-")
	if err != nil {
		return withFix(err, "free some disk space and run bqto update again")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	archive, err := u.Download(ctx, r, dir)
	if err != nil {
		return withFix(err, "check your network and run bqto update again")
	}
	if filepath.Dir(archive) != dir {
		return fmt.Errorf("the download of %s %s landed outside bqto's temporary folder, so bqto was not replaced\nFix: download the release by hand, as the README shows", u.Name, r.Version)
	}
	if err := Verify(archive, filepath.Join(dir, "checksums.txt")); err != nil {
		return withFix(err, "run bqto update again, or download the release by hand, as the README shows under Build and install")
	}
	if err := ctx.Err(); err != nil {
		return withFix(err, "run bqto update again")
	}
	if elevate {
		if err := sudoReplace(ctx, o, archive, dir); err != nil {
			return err
		}
	} else if err := Replace(archive, o.Exe); err != nil {
		return withFix(err, "run bqto update again, or download the release by hand, as the README shows under Build and install")
	}
	fmt.Fprintf(o.Out, "Updated %s from %s to %s: %s\n", u.Name, shown(have), r.Version, o.Exe)
	return nil
}

func needsSudo(o Options) (bool, error) {
	err := CheckWritable(o.Exe, o.Updater.DirFix)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, errNoWrite) || o.Sudo.Root || o.Sudo.Run == nil {
		return false, err
	}
	dir := filepath.Dir(o.Exe)
	if !o.Sudo.Found {
		return false, fmt.Errorf("%w: bqto needs sudo to replace %s, because you cannot write to %s, but sudo is not on your PATH\nFix: %s", errNoWrite, o.Exe, dir, o.Updater.DirFix)
	}
	if !o.Sudo.Terminal {
		return false, fmt.Errorf("%w: bqto needs sudo to replace %s, because you cannot write to %s, but there is no terminal for sudo to ask for your password\nFix: run bqto update in a terminal, or %s", errNoWrite, o.Exe, dir, o.Updater.DirFix)
	}
	return true, nil
}

func sudoReplace(ctx context.Context, o Options, archive, dir string) error {
	mode := modeOf(o.Exe)
	staged, err := extract(archive, dir, mode)
	if err != nil {
		return withFix(err, "run bqto update again, or download the release by hand, as the README shows under Build and install")
	}
	uid, gid, err := ownerOf(o.Exe)
	if err != nil {
		return withFix(err, "download the release by hand, as the README shows under Build and install")
	}
	fmt.Fprintf(o.Err, "bqto needs sudo to replace %s\n", o.Exe)
	argv := SudoCommand(staged, o.Exe, mode, uid, gid)
	if err := o.Sudo.Run(context.WithoutCancel(ctx), argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("sudo did not replace %s (%w), so the old binary stays\nFix: run bqto update again and enter your password, or %s", o.Exe, err, o.Updater.DirFix)
	}
	return nil
}

func withFix(err error, fix string) error {
	if strings.Contains(err.Error(), "\nFix: ") {
		return err
	}
	return fmt.Errorf("%w\nFix: %s", err, fix)
}
