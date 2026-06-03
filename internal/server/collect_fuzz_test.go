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
			t.Fatalf("truncLabel result len %d exceeds max %d", len(got), maxLabelLen)
		}
		if utf8.ValidString(input) && !utf8.ValidString(got) {
			t.Fatalf("truncLabel produced invalid UTF-8 from valid input")
		}
	})
}
