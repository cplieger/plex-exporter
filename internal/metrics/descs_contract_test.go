package metrics_test

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/plex-exporter/internal/metrics"
)

const familiesGolden = "testdata/families.golden"

// Every family keeps the name, type, HELP and label names it was released
// with. A changed or removed line breaks the queries, dashboards and rules
// built on it, so it is a breaking release; an added line is a new family.
// Regenerate with UPDATE_GOLDEN=1 go test ./internal/metrics/ -run TestAllDescs_keep_their_released_contract
func TestAllDescs_keep_their_released_contract(t *testing.T) {
	var b strings.Builder
	for _, d := range metrics.AllDescs {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", d.Name, d.Type, strings.Join(d.Labels, ","), d.Help)
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(familiesGolden, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write %s: %v", familiesGolden, err)
		}
		return
	}
	want, err := os.ReadFile(familiesGolden)
	if err != nil {
		t.Fatalf("read %s: %v; regenerate with UPDATE_GOLDEN=1 go test ./internal/metrics/ -run TestAllDescs_keep_their_released_contract", familiesGolden, err)
	}
	if !bytes.Equal([]byte(b.String()), want) {
		t.Errorf("AllDescs differ from %s; a changed or removed family is a breaking release, an added one a new family. Regenerate with UPDATE_GOLDEN=1 go test ./internal/metrics/ -run TestAllDescs_keep_their_released_contract\ngot:\n%s", familiesGolden, b.String())
	}
}

// The families that existed before the catalog walk keep their label sets
// exactly, because a changed label set breaks every stored query.
func TestAllDescs_existing_families_keep_their_labels(t *testing.T) {
	want := map[string]int{
		"plex_server_info": 6, "plex_library_storage_bytes": 5, "plex_library_items": 6,
		"plex_plays_active": 21, "plex_play_seconds_total": 21, "plex_exporter_errors_total": 1,
	}
	for _, d := range metrics.AllDescs {
		if n, ok := want[d.Name]; ok && len(d.Labels) != n {
			t.Errorf("%s has %d labels %v, want %d", d.Name, len(d.Labels), d.Labels, n)
		}
	}
}

// Item identity labels are allowed only on the families whose population is
// a fixed top-N; the free-text library name only on the families that had it.
func TestAllDescs_identity_labels_only_on_top_n_families(t *testing.T) {
	topN := []string{
		"plex_library_top_item_bytes",
		"plex_library_top_item_last_played_timestamp_seconds",
		"plex_library_recent_item_added_timestamp_seconds",
	}
	withLibrary := []string{
		"plex_library_duration_milliseconds", "plex_library_storage_bytes", "plex_library_items",
		"plex_plays_active", "plex_play_seconds_total",
	}
	seen := 0
	for _, d := range metrics.AllDescs {
		name, labels := d.Name, d.Labels
		if slices.Contains(topN, name) {
			seen++
			for _, l := range []string{"rating_key", "title", "server_id", "library_id"} {
				if !slices.Contains(labels, l) {
					t.Errorf("%s labels %v lack %q", name, labels, l)
				}
			}
		} else if slices.Contains(labels, "rating_key") {
			t.Errorf("%s carries rating_key but is not a top-N family", name)
		}
		if slices.Contains(labels, "library") && !slices.Contains(withLibrary, name) {
			t.Errorf("%s carries the free-text library label", name)
		}
	}
	if seen != len(topN) {
		t.Errorf("found %d of the %d top-N families in AllDescs", seen, len(topN))
	}
}

func TestResolutionBucket(t *testing.T) {
	tests := map[string]string{
		"": "unknown", " ": "unknown", "4k": "2160", "4K": "2160", "2160": "2160",
		"1080": "1080", "SD": "sd", "8k": "other", "1440": "other",
	}
	for in, want := range tests {
		if got := metrics.ResolutionBucket(in); got != want {
			t.Errorf("ResolutionBucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeCodec(t *testing.T) {
	tests := map[string]string{
		"": "unknown", "H264": "h264", "hevc": "hevc", "av1": "av1", "prores": "other",
	}
	for in, want := range tests {
		if got := metrics.NormalizeCodec(in); got != want {
			t.Errorf("NormalizeCodec(%q) = %q, want %q", in, got, want)
		}
	}
}
