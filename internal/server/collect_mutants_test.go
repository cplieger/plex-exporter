package server

import (
	"strings"
	"testing"
)

// TestTruncLabel_all_continuation_bytes_walks_to_zero forces truncLabel's
// UTF-8 boundary-walk loop all the way down to i==0. A string longer than
// maxLabelLen made entirely of continuation bytes (0x80) is never a rune
// start, so the loop decrements i from maxLabelLen down to 0. The guard must
// stop AT zero (`i > 0`); a `>=` boundary mutant would evaluate s[0], step to
// i=-1, and panic on s[:-1]. Plex API label strings are user-controlled and
// may be malformed UTF-8, so this is a real input class.
func TestTruncLabel_all_continuation_bytes_walks_to_zero(t *testing.T) {
	input := strings.Repeat("\x80", maxLabelLen+1)

	got := truncLabel(input)

	if got != "" {
		t.Errorf("truncLabel(all-continuation-bytes len=%d) = %q (len=%d), want %q",
			len(input), got, len(got), "")
	}
}
