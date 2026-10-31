package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgetBastion(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	certs := filepath.Join(ssh, "google_compute_engine_cert")
	if err := os.MkdirAll(certs, 0o700); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(ssh, "google_compute_known_hosts")
	hosts := "compute.111 ssh-ed25519 AAAA1\ncompute.4242 ssh-ed25519 AAAA2\ncompute.4242 ecdsa-sha2-nistp256 AAAA3\ncompute.42420 ssh-ed25519 AAAA4\n"
	if err := os.WriteFile(known, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(certs, "p_us-central1-a_4242-cert.pub")
	other := filepath.Join(certs, "p_us-central1-a_111-cert.pub")
	for _, f := range []string{cert, other} {
		if err := os.WriteFile(f, []byte("cert"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ForgetBastion(home, "p", "us-central1-a", 4242); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	if want := "compute.111 ssh-ed25519 AAAA1\ncompute.42420 ssh-ed25519 AAAA4\n"; string(got) != want {
		t.Errorf("known_hosts =\n%s\nwant\n%s", got, want)
	}
	if fi, _ := os.Stat(known); fi.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode = %v, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(cert); !os.IsNotExist(err) {
		t.Error("the bastion's certificate is still there")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("another bastion's certificate was removed")
	}
	if err := ForgetBastion(home, "p", "us-central1-a", 4242); err != nil {
		t.Errorf("a second call, with nothing left to remove: %v", err)
	}
	if err := ForgetBastion(t.TempDir(), "p", "z", 1); err != nil {
		t.Errorf("no ~/.ssh at all: %v", err)
	}
}

func TestForgetBastionLeavesNoTempFile(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "google_compute_known_hosts"), []byte("compute.7 ssh-ed25519 A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ForgetBastion(home, "p", "z", 7); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(ssh)
	if len(entries) != 1 {
		t.Errorf("~/.ssh holds %d entries, want only known_hosts", len(entries))
	}
	if fi, _ := os.Stat(filepath.Join(ssh, "google_compute_known_hosts")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want the original 0644", fi.Mode().Perm())
	}
}

func TestForgetBastionKeepsLongLines(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(ssh, "google_compute_known_hosts")
	long := "compute.1 ssh-rsa " + strings.Repeat("A", 100_000)
	if err := os.WriteFile(known, []byte("compute.7 ssh-ed25519 A\n"+long+"\ncompute.2 ssh-ed25519 B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ForgetBastion(home, "p", "z", 7); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	if want := long + "\ncompute.2 ssh-ed25519 B\n"; string(got) != want {
		t.Errorf("known_hosts lost lines: got %d bytes, want %d", len(got), len(want))
	}
}

func TestForgetBastionWritesThroughASymlink(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(hosts, []byte("compute.7 ssh-ed25519 A\ncompute.2 ssh-ed25519 B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ssh, "google_compute_known_hosts")
	if err := os.Symlink(hosts, link); err != nil {
		t.Fatal(err)
	}
	if err := ForgetBastion(home, "p", "z", 7); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("known_hosts is no longer a symlink: %v", err)
	}
	got, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatal(err)
	}
	if want := "compute.2 ssh-ed25519 B\n"; string(got) != want {
		t.Errorf("symlink target =\n%s\nwant\n%s", got, want)
	}
}
