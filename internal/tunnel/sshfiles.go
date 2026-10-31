package tunnel

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func ForgetBastion(home, project, zone string, id uint64) error {
	cert := filepath.Join(home, ".ssh", "google_compute_engine_cert", fmt.Sprintf("%s_%s_%d-cert.pub", project, zone, id))
	certErr := os.Remove(cert)
	if errors.Is(certErr, fs.ErrNotExist) {
		certErr = nil
	}
	return errors.Join(certErr, dropHostKeys(filepath.Join(home, ".ssh", "google_compute_known_hosts"), fmt.Sprintf("compute.%d", id)))
}

func dropHostKeys(path, alias string) error {
	path, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for range 3 {
		done, err := tryDropHostKeys(path, alias)
		if err != nil || done {
			return err
		}
	}
	return fmt.Errorf("%s kept changing while bqto removed %s", path, alias)
}

func tryDropHostKeys(path, alias string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the user's ssh known_hosts or key file
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var kept []string
	dropped := false
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSuffix(line, "\n")
		if host, _, _ := strings.Cut(line, " "); host == alias {
			dropped = true
			continue
		}
		kept = append(kept, line)
	}
	if !dropped {
		return true, nil
	}
	out := strings.Join(kept, "\n")
	if len(kept) > 0 {
		out += "\n"
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename; a leftover temp file is harmless
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return false, err
	}
	now, err := os.ReadFile(path) //nolint:gosec // path is the user's ssh known_hosts or key file
	if err != nil || string(now) != string(data) {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}
