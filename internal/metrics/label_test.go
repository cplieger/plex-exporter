package metrics_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"pgregory.net/rapid"
)

func TestTruncateLabelValue(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{name: "shorter than cap returned unchanged", in: "hello", maxBytes: 10, want: "hello"},
		{name: "equal to cap returned unchanged", in: "hello", maxBytes: 5, want: "hello"},
		{name: "ascii truncated at exact byte", in: "hello", maxBytes: 3, want: "hel"},
		{name: "cap of zero yields empty", in: "hello", maxBytes: 0, want: ""},
		{name: "empty input yields empty", in: "", maxBytes: 4, want: ""},
		{name: "multibyte rune not split mid-sequence", in: "héllo", maxBytes: 2, want: "h"},
		{name: "cut on a rune boundary keeps whole runes", in: "héllo", maxBytes: 3, want: "hé"},
		{name: "three-byte runes walk back to boundary", in: "日本語", maxBytes: 4, want: "日"},
		{name: "single rune wider than cap yields empty", in: "𐀀", maxBytes: 2, want: ""},
		{name: "invalid utf-8 of only continuation bytes yields empty", in: string([]byte{0x80, 0x80, 0x80}), maxBytes: 2, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metrics.TruncateLabelValue(tt.in, tt.maxBytes); got != tt.want {
				t.Errorf("TruncateLabelValue(%q, %d) = %q, want %q", tt.in, tt.maxBytes, got, tt.want)
			}
		})
	}
}

func TestTruncateLabelValue_properties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		maxBytes := rapid.IntRange(0, 64).Draw(t, "maxBytes")
		got := metrics.TruncateLabelValue(s, maxBytes)

		if len(got) > maxBytes {
			t.Fatalf("TruncateLabelValue(%q, %d) = %q (%d bytes), exceeds cap", s, maxBytes, got, len(got))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("TruncateLabelValue(%q, %d) = %q is not a prefix of the input", s, maxBytes, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateLabelValue(%q, %d) = %q is not valid UTF-8", s, maxBytes, got)
		}
		if again := metrics.TruncateLabelValue(got, maxBytes); again != got {
			t.Fatalf("not idempotent: TruncateLabelValue(%q,%d)=%q then =%q", s, maxBytes, got, again)
		}
	})
}
