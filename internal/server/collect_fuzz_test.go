package server

import (
	"testing"
	"unicode/utf8"
)

func FuzzTruncLabel(f *testing.F) {
	f.Add("hello")
	f.Add("")
	f.Add("a]ñ日本語" + string(make([]byte, 200)))

	f.Fuzz(func(t *testing.T, input string) {
		got := truncLabel(input)
		if len(got) > maxLabelLen {
			t.Fatalf("truncLabel(%q) result len %d exceeds max %d", input, len(got), maxLabelLen)
		}
		if utf8.ValidString(input) && !utf8.ValidString(got) {
			t.Fatalf("truncLabel(%q) produced invalid UTF-8 %q from valid input", input, got)
		}
		// Idempotent: an already-bounded label must pass through unchanged.
		if got2 := truncLabel(got); got2 != got {
			t.Fatalf("truncLabel not idempotent: truncLabel(%q)=%q, truncLabel(%q)=%q",
				input, got, got, got2)
		}
	})
}
