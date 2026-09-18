package metrics_test

import (
	"testing"

	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// The exporter's own metrics describe its link to the one configured server,
// so they carry no server identity: that keeps each on a single series from
// the first scrape, before Plex has answered.
func TestExporterDescs_take_only_their_own_labels(t *testing.T) {
	tests := []struct {
		name   string
		desc   *prometheus.Desc
		labels []string
	}{
		{"http_reachable", metrics.DescHTTPReachable, nil},
		{"session_poll_reachable", metrics.DescSessionPollReachable, nil},
		{"http_retries", metrics.DescHTTPRetries, nil},
		{"errors", metrics.DescErrors, []string{"refresh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := prometheus.NewConstMetric(tt.desc, prometheus.GaugeValue, 1, tt.labels...); err != nil {
				t.Errorf("NewConstMetric(%s, %d labels) error = %v, want nil", tt.name, len(tt.labels), err)
			}
			withIdentity := append([]string{"srv", "id"}, tt.labels...)
			if _, err := prometheus.NewConstMetric(tt.desc, prometheus.GaugeValue, 1, withIdentity...); err == nil {
				t.Errorf("NewConstMetric(%s, %d labels) error = nil, want a label-count mismatch", tt.name, len(withIdentity))
			}
		})
	}
}
