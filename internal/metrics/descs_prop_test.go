package metrics_test

import (
	"testing"

	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"pgregory.net/rapid"
)

// For any input, including attacker-controlled strings from a compromised
// Plex server, Normalize must return an allowlisted value or the Fallback,
// and the mapping must be idempotent.
func TestNormalize_bounded_to_allowlist(t *testing.T) {
	lists := []*metrics.LabelAllowlist{
		metrics.StreamTypeAllowlist,
		metrics.MediaTypeAllowlist,
		metrics.ResolutionAllowlist,
	}
	for _, list := range lists {
		t.Run(list.Name, func(t *testing.T) {
			rapid.Check(t, func(t *rapid.T) {
				v := rapid.String().Draw(t, "v")
				got := list.Normalize(v)
				if got != list.Fallback && !list.Allowed[got] {
					t.Fatalf("Normalize(%q) = %q, neither an allowlisted value nor the fallback %q", v, got, list.Fallback)
				}
				if again := list.Normalize(got); again != got {
					t.Fatalf("Normalize not idempotent: Normalize(%q) = %q, Normalize(%q) = %q", v, got, got, again)
				}
			})
		})
	}
}

func TestNormalize_case_insensitive_for_known_values(t *testing.T) {
	tests := []struct {
		name string
		list *metrics.LabelAllowlist
		in   string
		want string
	}{
		{"stream upper", metrics.StreamTypeAllowlist, "TRANSCODE", "transcode"},
		{"stream mixed", metrics.StreamTypeAllowlist, "DirectPlay", "directplay"},
		{"media upper", metrics.MediaTypeAllowlist, "EPISODE", "episode"},
		{"resolution upper", metrics.ResolutionAllowlist, "4K", "4k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.list.Normalize(tt.in); got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
