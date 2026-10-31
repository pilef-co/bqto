package tunnel

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func ReservePort(dir string, want int) (int, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // dir is bqto's cache directory
		return 0, nil, fmt.Errorf("reserve a local port: %w\nFix: check that %s can be created", err, dir)
	}
	for p := want; p < want+PortSearchRange; p++ {
		ln, err := Listen(p)
		if err != nil {
			continue
		}
		_ = ln.Close()
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("port-%d.lock", p)), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // dir is bqto's cache directory
		if err != nil {
			return 0, nil, fmt.Errorf("reserve a local port: %w\nFix: check that %s is writable", err, dir)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			continue
		}
		return p, func() { _ = f.Close() }, nil
	}
	return 0, nil, fmt.Errorf("no free local port from %d to %d\nFix: free one of them", want, want+PortSearchRange-1)
}

func ReservedPortsDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "bqto", "ports")
}
