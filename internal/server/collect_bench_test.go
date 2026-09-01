package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/v2/internal/library"
	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"github.com/cplieger/plex-exporter/v2/internal/sessions"
	"github.com/prometheus/client_golang/prometheus"
)

// These benchmarks feed the weekly benchmark tracker. Collect's cost scales
// with playing-session count; truncLabel runs once per label value on the
// same path.
//
// Both were dead until 2026-08: `go test ./...` compiles a benchmark but
// never runs one without -bench, so benchServer's nil-dereference (Item.Player/
// Session/User are pointers on plexapi.Item) went unnoticed. The fixture is
// built with testMeta, decoding real JSON, so it cannot diverge from the wire
// shape again.

// benchSessionJSON is one playing session as /status/sessions reports it:
// direct-play movie, LAN client, known bandwidth and bitrate.
const benchSessionJSON = `{
	"type":"movie",
	"title":"Bench Movie",
	"Media":[{"videoResolution":"1080","bitrate":8000,"Part":[{"decision":"copy"}]}],
	"Player":{"device":"Chrome","product":"Plex Web","local":true},
	"Session":{"location":"lan","bandwidth":5000},
	"User":{"title":"benchuser"}
}`

// benchMediaJSON is the cached /library/metadata item for that session.
const benchMediaJSON = `{
	"type":"movie",
	"title":"Bench Movie",
	"Media":[{"videoResolution":"1080"}]
}`

// benchServer returns a *Server pre-populated with n playing sessions.
func benchServer(tb testing.TB, n int) *Server {
	tb.Helper()
	tracker := sessions.NewTracker()
	meta := testMeta(tb, benchSessionJSON)
	mediaMeta := testMeta(tb, benchMediaJSON)
	now := time.Now()
	for i := range n {
		tracker.Sessions[fmt.Sprintf("s%d", i)] = sessions.Session{
			PlayStarted:    now.Add(-time.Duration(i+1) * time.Second),
			LastUpdate:     now,
			State:          sessions.StatePlaying,
			LibName:        "Movies",
			LibID:          "1",
			LibType:        library.TypeMovie,
			TranscodeType:  metrics.ValNone,
			SubtitleAction: metrics.ValNone,
			Meta:           meta,
			MediaMeta:      mediaMeta,
		}
	}
	return &Server{
		Name:     "BenchSrv",
		ID:       "bench-id",
		Version:  "1.0",
		Platform: "Linux",
		Sessions: tracker,
		Libraries: []library.Library{
			{Name: "Movies", ID: "1", Type: library.TypeMovie, DurationTotal: 100, StorageTotal: 200, ItemsCount: 50, ItemsKnown: true},
		},
		ErrorCounts: make(map[string]float64, len(metrics.ErrorTypes)),
	}
}

// BenchmarkCollect measures one scrape across session counts. The counts are
// spread so a per-session cost that turns super-linear shows as a widening
// gap between them rather than a uniform slowdown that reads as runner noise.
func BenchmarkCollect(b *testing.B) {
	for _, n := range []int{0, 5, 20} {
		srv := benchServer(b, n)

		// Channel sized from a real Collect: Collect sends synchronously, so
		// an undersized buffer deadlocks the benchmark instead of failing it.
		probe := make(chan prometheus.Metric, 4096)
		srv.Collect(probe)
		emitted := len(probe)
		for len(probe) > 0 {
			<-probe
		}
		if emitted == 0 {
			b.Fatalf("benchServer(%d): Collect emitted 0 metrics, want more than 0", n)
		}

		b.Run(fmt.Sprintf("sessions_%d", n), func(b *testing.B) {
			ch := make(chan prometheus.Metric, emitted)
			b.ReportAllocs()
			for b.Loop() {
				srv.Collect(ch)
				for len(ch) > 0 {
					<-ch
				}
			}
		})
	}
}

// BenchmarkTruncLabel covers the label-capping helper. It runs once per label
// value on the scrape path, so its cost multiplies by label count; the two
// cases separate the under-cap fast path from the cut.
func BenchmarkTruncLabel(b *testing.B) {
	cases := []struct {
		name  string
		value string
	}{
		{"short", "short"},
		{"long", strings.Repeat("x", 256)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				labelSink = truncLabel(tc.value)
			}
		})
	}
}

// labelSink keeps truncLabel's result live so the compiler cannot delete the
// call it is meant to measure.
var labelSink string
