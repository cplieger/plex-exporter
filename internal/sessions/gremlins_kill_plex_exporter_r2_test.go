package sessions

import (
	"strings"
	"testing"
)

// TestGkPlexExporterR2_normalizeKey_boundary pins normalizeKey's clamp at the
// MaxSessionKeyLen boundary (tracker.go:136). normalizeKey now clamps the
// slice bound with the min builtin instead of an `if len > cap` branch, so
// this test guards the three regions against a future regression (a dropped
// clamp, or an off-by-one cap):
//
//   - below cap:  returned unchanged
//   - AT the cap: returned unchanged — a MaxSessionKeyLen-byte key is exactly
//     the limit, not truncated. This is the boundary the prior `>` guard could
//     not be distinguished from a `>=` mutant on, because id[:N] == id when
//     len(id) == N; the min form removes that comparison outright.
//   - above cap:  truncated to exactly MaxSessionKeyLen bytes
func TestGkPlexExporterR2_normalizeKey_boundary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"below cap unchanged", strings.Repeat("a", MaxSessionKeyLen-1), strings.Repeat("a", MaxSessionKeyLen-1)},
		{"at cap unchanged", strings.Repeat("b", MaxSessionKeyLen), strings.Repeat("b", MaxSessionKeyLen)},
		{"above cap truncated", strings.Repeat("c", MaxSessionKeyLen+1), strings.Repeat("c", MaxSessionKeyLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeKey(tc.in)
			if got != tc.want {
				t.Errorf("normalizeKey(len=%d) = %q (len %d), want %q (len %d)",
					len(tc.in), got, len(got), tc.want, len(tc.want))
			}
		})
	}
}
