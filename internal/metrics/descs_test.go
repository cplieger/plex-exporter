package metrics_test

import (
	"slices"
	"testing"

	"github.com/cplieger/plex-exporter/internal/metrics"
)

// The exporter's own metrics describe its link to the one configured server,
// so they carry no server identity: that keeps each on a single series from
// the first scrape, before Plex has answered.
func TestExporterDescs_take_only_their_own_labels(t *testing.T) {
	tests := []struct {
		name   string
		desc   *metrics.Desc
		labels []string
	}{
		{"http_reachable", metrics.DescHTTPReachable, nil},
		{"session_poll_reachable", metrics.DescSessionPollReachable, nil},
		{"http_retries", metrics.DescHTTPRetries, nil},
		{"errors", metrics.DescErrors, []string{"type"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !slices.Equal(tt.desc.Labels, tt.labels) {
				t.Errorf("%s labels = %v, want %v", tt.desc.Name, tt.desc.Labels, tt.labels)
			}
		})
	}
}
