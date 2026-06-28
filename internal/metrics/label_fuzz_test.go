package metrics_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/plex-exporter/internal/metrics"
)

func FuzzTruncateLabelValue(f *testing.F) {
	f.Add("", 0)
	f.Add("hello", 3)
	f.Add("héllo", 2)
	f.Add("日本語", 4)
	f.Add("𐀀", 2)
	f.Add("ascii", 100)
	f.Add(string([]byte{0xff, 0xfe}), 1)
	f.Fuzz(func(t *testing.T, s string, maxBytes int) {
		if maxBytes < 0 {
			return
		}
		got := metrics.TruncateLabelValue(s, maxBytes)
		if len(got) > maxBytes {
			t.Fatalf("TruncateLabelValue(%q, %d) = %q (%d bytes), exceeds cap", s, maxBytes, got, len(got))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("TruncateLabelValue(%q, %d) = %q is not a prefix of the input", s, maxBytes, got)
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("valid input %q truncated to invalid UTF-8 %q at cap %d", s, got, maxBytes)
		}
		if again := metrics.TruncateLabelValue(got, maxBytes); again != got {
			t.Fatalf("not idempotent: TruncateLabelValue(%q,%d)=%q then =%q", s, maxBytes, got, again)
		}
	})
}
