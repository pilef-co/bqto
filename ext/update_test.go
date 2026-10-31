package ext

import "testing"

func TestNewerVersion(t *testing.T) {
	tests := []struct {
		latest, have string
		want         bool
	}{
		{"v0.12.0", "v0.11.0", true},
		{"0.12.0", "v0.11.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.11.0", "v0.11.0", false},
		{"v0.10.0", "v0.11.0", false},
		{"v0.11.1", "v0.11.1-0.20261027120000-abcdef123456", true},
		{"v0.11.0", "v0.11.1-0.20261027120000-abcdef123456", false},
		{"0.9.0-rc.1", "v0.8.0", true},
		{"v0.12.0", "dev", false},
		{"dev", "v0.11.0", false},
		{"v0.12", "v0.11.0", false},
		{"v0.12.x", "v0.11.0", false},
		{"v0.-1.0", "v0.0.0", false},
		{"v0.+1.0", "v0.0.0", false},
	}
	for _, tc := range tests {
		if got := NewerVersion(tc.latest, tc.have); got != tc.want {
			t.Errorf("NewerVersion(%q, %q) = %v, want %v", tc.latest, tc.have, got, tc.want)
		}
	}
}

func TestShortVersion(t *testing.T) {
	for in, want := range map[string]string{"v0.11.0": "0.11.0", "0.7.0-beta.1-12345": "0.7.0", "dev": "dev", "v01.2.3": "1.2.3"} {
		if got := ShortVersion(in); got != want {
			t.Errorf("ShortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
