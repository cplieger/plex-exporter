package metrics_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var descParts = regexp.MustCompile(`fqName: "([^"]+)".*variableLabels: \{([^}]*)\}`)

func descLabels(t *testing.T, d *prometheus.Desc) (name string, labels []string) {
	t.Helper()
	m := descParts.FindStringSubmatch(d.String())
	if m == nil {
		t.Fatalf("cannot parse descriptor %s", d.String())
	}
	if m[2] != "" {
		labels = strings.Split(m[2], ",")
	}
	return m[1], labels
}

// The families that existed before the catalog walk keep their label sets
// exactly, because a changed label set breaks every stored query.
func TestAllDescs_existing_families_keep_their_labels(t *testing.T) {
	want := map[string]int{
		"plex_server_info": 6, "plex_library_storage_bytes": 5, "plex_library_items": 6,
		"plex_plays_active": 21, "plex_play_seconds_total": 21, "plex_exporter_errors_total": 1,
	}
	for _, d := range metrics.AllDescs {
		name, labels := descLabels(t, d)
		if n, ok := want[name]; ok && len(labels) != n {
			t.Errorf("%s has %d labels %v, want %d", name, len(labels), labels, n)
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
		name, labels := descLabels(t, d)
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
