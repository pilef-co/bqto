package tunnel

import (
	"slices"
	"testing"
)

func FuzzStderrRingChunking(f *testing.F) {
	f.Add([]byte("debug1: Local forwarding listening on 127.0.0.1 port 15432.\nbind: Address already in use\n"), uint8(3))
	f.Add([]byte("no newline at the end"), uint8(1))
	f.Add([]byte("kex_exchange_identification: Connection closed by remote host\r\nERROR: exited\n"), uint8(5))
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		size := int(chunk%16) + 1
		whole := &stderrRing{max: 5}
		_, _ = whole.Write(data)
		pieces := &stderrRing{max: 5}
		for i := 0; i < len(data); i += size {
			_, _ = pieces.Write(data[i:min(i+size, len(data))])
		}
		if !slices.Equal(whole.lines(), pieces.lines()) {
			t.Errorf("chunks of %d give %q, one write gives %q", size, pieces.lines(), whole.lines())
		}
	})
}

func TestStderrRingDropsCarriageReturns(t *testing.T) {
	const cause = "kex_exchange_identification: Connection closed by remote host"
	const gcloudErr = "ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255]."
	for _, tc := range []struct {
		desc   string
		writes []string
		want   []string
	}{
		{
			desc:   "ssh ends its lines with CRLF",
			writes: []string{"debug1: Connecting to compute.123 port 22.\r\n", cause + "\r\n", gcloudErr + "\n"},
			want:   []string{"debug1: Connecting to compute.123 port 22.", cause, gcloudErr},
		},
		{
			desc:   "a CRLF line split across writes",
			writes: []string{"kex_exchange_identification: Conne", "ction closed by remote host\r", "\n", gcloudErr + "\n"},
			want:   []string{cause, gcloudErr},
		},
		{
			desc:   "LF lines stay as they are",
			writes: []string{cause + "\n", gcloudErr + "\n"},
			want:   []string{cause, gcloudErr},
		},
		{
			desc:   "a blank line stays",
			writes: []string{"a\n\nb\n"},
			want:   []string{"a", "", "b"},
		},
		{
			desc:   "a lone CR inside a line",
			writes: []string{"progress 10%\rprogress 100%\n"},
			want:   []string{"progress 10%progress 100%"},
		},
		{
			desc:   "an unfinished last line",
			writes: []string{cause + "\r"},
			want:   []string{cause},
		},
	} {
		r := &stderrRing{max: 60}
		for _, w := range tc.writes {
			_, _ = r.Write([]byte(w))
		}
		if got := r.lines(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: lines = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

func TestRetryReasonShowsTheWholeSSHCause(t *testing.T) {
	r := &stderrRing{max: 60}
	for _, w := range []string{
		"debug1: Connecting to compute.123 port 22.\r\n",
		"kex_exchange_identification: Connection closed by remote host\r\n",
		"Connection closed by UNKNOWN port 65535\r\n",
		"ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255].\n",
	} {
		_, _ = r.Write([]byte(w))
	}
	want := "kex_exchange_identification: Connection closed by remote host | Connection closed by UNKNOWN port 65535 | ERROR: (gcloud.compute.ssh) [/usr/bin/ssh] exited with return code [255]."
	if got := retryReason(WithoutDebug(r.lines()), nil); got != want {
		t.Errorf("retryReason = %q, want %q", got, want)
	}
}
