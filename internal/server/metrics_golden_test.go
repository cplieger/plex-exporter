package server

import (
	"bytes"
	"context"
	"iter"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/libstats"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/cplieger/plexapi/v3"
)

const metricsGolden = "testdata/metrics.golden"

// fixedHistory serves the same watch-history rows on every read.
type fixedHistory []plexapi.HistoryEntry

func (h fixedHistory) WalkHistory(context.Context, int64, plexapi.Page, func(context.Context) error) iter.Seq2[plexapi.HistoryEntry, error] {
	return func(yield func(plexapi.HistoryEntry, error) bool) {
		for _, e := range h {
			if !yield(e, nil) {
				return
			}
		}
	}
}

// goldenServer returns a Server whose state makes the collector emit every
// family in metrics.AllDescs. It must run inside a synctest bubble, whose
// clock starts at the same instant on every run, so every timestamp and
// play time on the page is fixed.
func goldenServer(t *testing.T) *Server {
	t.Helper()
	srv := New(nil)
	srv.Pace = nil
	meta := testMeta(t, `{"sessionKey":"s1","ratingKey":"10","type":"movie","title":"Movie Ten",
		"User":{"title":"ann \"a\\b\""},"Player":{"device":"TV","product":"Plex for LG","local":true},
		"Session":{"bandwidth":9000,"location":"lan"},
		"Media":[{"bitrate":8000,"videoResolution":"1080","Part":[{"decision":"transcode"}]}]}`)
	media := testMeta(t, `{"ratingKey":"10","type":"movie","title":"Movie Ten","librarySectionID":1,
		"Media":[{"videoResolution":"4k"}]}`)
	srv.Sessions.Update("s1", sessions.StatePlaying, &meta, &media)
	srv.Sessions.UpdateLibraryLabels("s1", func(ss *sessions.Session) {
		ss.TranscodeType, ss.SubtitleAction = "video", "burn"
		ss.VideoTranscoding, ss.VideoDecode, ss.VideoEncode = true, "hardware", "software"
		ss.SourceVideoCodec, ss.TargetVideoCodec = "hevc", "h264"
	})
	ep := testMeta(t, `{"sessionKey":"s2","ratingKey":"21","type":"episode","grandparentTitle":"Show","parentTitle":"Season 2","title":"Pilot","index":3}`)
	srv.Sessions.Update("s2", sessions.StatePlaying, &ep, &ep)
	time.Sleep(90 * time.Second)
	srv.Sessions.Update("s2", sessions.StatePaused, nil, nil)
	time.Sleep(30 * time.Second)
	now := time.Now()
	srv.History = history.New(fixedHistory{
		{RatingKey: "10", HistoryKey: "h1", ViewedAt: now.Add(-48 * time.Hour).Unix()},
		{RatingKey: "11", HistoryKey: "h2", ViewedAt: now.Add(-400 * 24 * time.Hour).Unix()},
		{HistoryKey: "h3", ViewedAt: now.Add(-time.Hour).Unix()},
	}, noPace{})
	if st := srv.History.Bootstrap(t.Context()); st != history.StatusComplete {
		t.Fatalf("Setup: Bootstrap() = %q, want complete", st)
	}

	srv.Name, srv.ID, srv.Version = `Home "Plex"\Server`, "mid1", "1.41.0"
	srv.Platform, srv.PlatformVersion, srv.PlexPass = "Linux", "6.1", true
	srv.HTTPReachable, srv.SessionsReachable = true, true
	srv.HostCPU, srv.HostMem, srv.ResourcesRead = 0.42, 0.655, true
	srv.TransmitBytes, srv.BandwidthRead = 123456789, true
	srv.ActiveTranscodes = 1
	srv.ErrorCounts["refresh"], srv.ErrorCounts["history_fetch"] = 3, 1
	srv.deadlineMisses = map[string]float64{"bandwidth": 2}
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie, DurationTotal: 7200000, StorageTotal: 5000000000, ItemsCount: 2, ItemsKnown: true},
		{ID: "2", Name: "TV\nShows", Type: library.TypeShow, DurationTotal: 360000, StorageTotal: 900000, ItemsCount: 0, ItemsKnown: true},
		{ID: "3", Name: "Music", Type: library.TypeArtist, DurationTotal: 1000, StorageTotal: 2000},
	}

	stamp := srv.History.View().Gen
	srv.walks = walkState{
		libs: map[string]*libWalk{
			"1": {
				lastSuccess: now.Add(-10 * time.Minute), duration: 1500 * time.Millisecond, stamp: stamp, walked: true,
				stats: libstats.Stats{
					ResolutionBytes:   map[string]int64{"1080": 3000, "2160": 2000},
					CodecBytes:        map[string]int64{"h264": 3000, "hevc": 2000},
					WatchAgeBytes:     map[string]int64{libstats.WatchedNever: 1000, libstats.WatchedUnder90d: 4000},
					WatchAgeItems:     map[string]int64{libstats.WatchedNever: 1, libstats.WatchedUnder90d: 1},
					MultiVersionItems: 1, ExtraVersionBytes: 1500,
					NewestAdded: now.Add(-24 * time.Hour).Unix(), Added24h: 0, Added7d: 2, Unsized: 1,
					Largest: []libstats.Item{
						{RatingKey: 11, Title: "Movie \"Eleven\"", Year: 2001, Bytes: 3000, LastPlayed: now.Add(-400 * 24 * time.Hour).Unix()},
						{RatingKey: 10, Title: "Movie Ten", Year: 0, Bytes: 2000},
					},
					Recent: []libstats.Item{
						{LibraryID: "1", LibraryType: library.TypeMovie, RatingKey: 10, Title: "Movie Ten", AddedAt: now.Add(-24 * time.Hour).Unix()},
					},
				},
			},
			"2": {
				lastSuccess: now.Add(-20 * time.Minute), duration: 90 * time.Second, stamp: stamp, walked: true,
				stats: libstats.Stats{
					Recent: []libstats.Item{
						{LibraryID: "2", LibraryType: library.TypeShow, RatingKey: 20, Title: "Show", Year: 2019, Episode: "S02E03", AddedAt: now.Add(-2 * time.Hour).Unix()},
					},
				},
			},
		},
		passDuration: 92 * time.Second,
		passDone:     true,
	}
	srv.background = backgroundState{
		sections:     []section{{id: "1", typ: library.TypeMovie, scannedAt: now.Add(-3 * time.Hour).Unix()}, {id: "2", typ: library.TypeShow}},
		activities:   map[activityKey]float64{{"library.update.section", library.TypeMovie, "1"}: 0.4, {"butler", "", ""}: -1},
		activitiesAt: now,
		update:       updateState{state: "available", checkedAt: now.Add(-6 * time.Hour).Unix(), available: 1, known: true},
		scanInterval: time.Hour,
		prefsAt:      now,
	}

	return srv
}

// exporterPage returns the /metrics page without the Go and process
// families, whose values move between runs.
func exporterPage(page string) string {
	var b strings.Builder
	for line := range strings.Lines(page) {
		name := strings.TrimPrefix(strings.TrimPrefix(line, "# HELP "), "# TYPE ")
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// The page the exporter serves for a fixed state is pinned byte for byte:
// every family, label order, escape and number format. Regenerate with
// UPDATE_GOLDEN=1 go test ./internal/server/ -run TestMetricsPage_matches_golden
// only for an intended change to the exposition, which is a breaking release.
func TestMetricsPage_matches_golden(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := exporterPage(servePage(t, goldenServer(t).Collector("v9.9.9")))
		for _, d := range metrics.AllDescs {
			if !strings.Contains(got, "\n# TYPE "+d.Name+" ") {
				t.Errorf("page lacks %s, want the fixture to emit every family", d.Name)
			}
		}

		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(metricsGolden, []byte(got), 0o600); err != nil {
				t.Fatalf("write %s: %v", metricsGolden, err)
			}
			return
		}
		want, err := os.ReadFile(metricsGolden)
		if err != nil {
			t.Fatalf("read %s: %v; regenerate with UPDATE_GOLDEN=1 go test ./internal/server/ -run TestMetricsPage_matches_golden", metricsGolden, err)
		}
		if !bytes.Equal([]byte(got), want) {
			t.Errorf("/metrics differs from %s; a changed byte breaks every scraper's queries. Regenerate with UPDATE_GOLDEN=1 go test ./internal/server/ -run TestMetricsPage_matches_golden only for an intended change\ngot:\n%s", metricsGolden, got)
		}
	})
}
