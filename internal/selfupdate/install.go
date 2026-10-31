package selfupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxBinary = 200 << 20

var errNoWrite = errors.New("no write access")

func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("could not find the running bqto: %w\nFix: download the release by hand, as the README shows under Build and install", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("could not follow %s to the bqto binary: %w\nFix: download the release by hand, as the README shows under Build and install", exe, err)
	}
	return resolved, nil
}

func CheckWritable(exe, fix string) error {
	f, err := os.CreateTemp(filepath.Dir(exe), ".bqto-update-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%w: bqto cannot replace %s, because you cannot write to %s\nFix: %s", errNoWrite, exe, filepath.Dir(exe), fix)
		}
		return fmt.Errorf("bqto cannot replace %s: %w\nFix: %s", exe, err, fix)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func Verify(archive, checksums string) error {
	want, err := expectedSum(checksums, filepath.Base(archive))
	if err != nil {
		return err
	}
	f, err := os.Open(archive) //nolint:gosec // archive is in bqto's own temporary folder
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the SHA-256 of %s is %s, but checksums.txt says %s, so bqto was not replaced\nFix: run bqto update again; if it fails the same way, report it as SECURITY.md says", filepath.Base(archive), got, want)
	}
	return nil
}

func expectedSum(checksums, name string) (string, error) {
	f, err := os.Open(checksums) //nolint:gosec // checksums.txt is in bqto's own temporary folder
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxChecksums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
			return "", fmt.Errorf("checksums.txt has no valid SHA-256 for %s, so bqto was not replaced\nFix: run bqto update again", name)
		}
		return sum, nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksums.txt has no line for %s, so bqto was not replaced\nFix: download the release by hand, as the README shows under Build and install", name)
}

func Replace(archive, exe string) error {
	staged, err := extract(archive, filepath.Dir(exe), modeOf(exe))
	if err != nil {
		return err
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("bqto could not replace %s: %w\nFix: download the release by hand, as the README shows under Build and install", exe, err)
	}
	return nil
}

func extract(archive, dir string, mode fs.FileMode) (string, error) {
	in, err := binaryIn(archive)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".bqto-update-*")
	if err != nil {
		return "", fmt.Errorf("bqto cannot write to %s: %w\nFix: download the release by hand, as the README shows under Build and install", dir, err)
	}
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(in, maxBinary+1))
	if err != nil {
		return "", err
	}
	if n > maxBinary {
		return "", fmt.Errorf("the bqto binary in %s is larger than %d bytes, so bqto was not replaced\nFix: download the release by hand, as the README shows under Build and install", filepath.Base(archive), maxBinary)
	}
	if err := tmp.Chmod(mode); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	done = true
	return tmp.Name(), nil
}

type tarFile struct {
	io.Reader
	closers []io.Closer
}

func (t tarFile) Close() error {
	var err error
	for _, c := range t.closers {
		err = errors.Join(err, c.Close())
	}
	return err
}

func binaryIn(archive string) (io.ReadCloser, error) {
	f, err := os.Open(archive) //nolint:gosec // archive is in bqto's own temporary folder
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a gzip archive: %w\nFix: run bqto update again", filepath.Base(archive), err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			_ = gz.Close()
			_ = f.Close()
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%s holds no bqto binary\nFix: download the release by hand, as the README shows under Build and install", filepath.Base(archive))
			}
			return nil, fmt.Errorf("%s is not a valid archive: %w\nFix: run bqto update again", filepath.Base(archive), err)
		}
		if strings.TrimPrefix(h.Name, "./") == "bqto" && h.Typeflag == tar.TypeReg {
			return tarFile{Reader: tr, closers: []io.Closer{gz, f}}, nil
		}
	}
}

func modeOf(exe string) fs.FileMode {
	fi, err := os.Stat(exe)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		return 0o755
	}
	return fi.Mode().Perm()
}
