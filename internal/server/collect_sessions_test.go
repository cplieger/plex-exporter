package server

import (
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectSessionsPlaying(t *testing.T) {
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{
		"Player":{"device":"Chrome","product":"Plex Web","local":true},
		"Session":{"location":"lan","bandwidth":5000},
		"User":{"title":"testuser"},
		"Media":[{"videoResolution":"1080","bitrate":8000,"Part":[{"decision":"copy"}]}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Test Movie","Media":[{"videoResolution":"1080"}]}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:    time.Now().Add(-10 * time.Second),
		LastUpdate:     time.Now(),
		State:          sessions.StatePlaying,
		LibName:        "Movies",
		LibID:          "1",
		LibType:        library.TypeMovie,
		Meta:           meta,
		MediaMeta:      mediaMeta,
		TranscodeType:  metrics.ValNone,
		SubtitleAction: metrics.ValNone,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	ms := drainMetrics(ch)
	// 4 metrics: play_count, play_seconds, session_bandwidth, session_bitrate
	if len(ms) != 4 {
		t.Errorf("collectSessions produced %d metrics, want 4", len(ms))
	}
}

func TestCollectSessionsSkipsZeroPlayStarted(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		State:      sessions.StatePlaying,
		LastUpdate: time.Now(),
		// playStarted is zero — should be skipped
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	ms := drainMetrics(ch)
	// 0 metrics — the session is skipped (zero playStarted) and nothing else is emitted
	if len(ms) != 0 {
		t.Errorf("collectSessions produced %d metrics, want 0", len(ms))
	}
}

func TestCollectSessionsLibraryLookup(t *testing.T) {
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user1"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Resolved Movie","librarySectionID":"3"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:    time.Now().Add(-5 * time.Second),
		LastUpdate:     time.Now(),
		State:          sessions.StateStopped,
		Meta:           meta,
		MediaMeta:      mediaMeta,
		PrevPlayedTime: 5 * time.Second,
	}

	libs := []library.Library{{ID: "3", Name: "4K Movies", Type: library.TypeMovie}}
	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", libs)
	close(ch)

	ms := drainMetrics(ch)
	// 2 metrics: play_count, play_seconds
	if len(ms) != 2 {
		t.Errorf("collectSessions produced %d metrics, want 2", len(ms))
	}
}

func TestCollectSessionsUnknownLibrary(t *testing.T) {
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{"Player":{"device":"Phone"},"User":{"title":"user2"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Unknown Lib Movie","librarySectionID":"999"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-1 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		Meta:        meta,
		MediaMeta:   mediaMeta,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	ms := drainMetrics(ch)
	// 2 metrics: play_count, play_seconds
	if len(ms) != 2 {
		t.Errorf("collectSessions produced %d metrics, want 2", len(ms))
	}
}

func TestCollectSessionsPendingTranscodeNormalized(t *testing.T) {
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user3"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Pending Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:   time.Now().Add(-1 * time.Second),
		LastUpdate:    time.Now(),
		State:         sessions.StatePlaying,
		TranscodeType: metrics.ValPending, // should be normalized to "none"
		LibName:       "Movies",
		LibID:         "1",
		LibType:       library.TypeMovie,
		Meta:          meta,
		MediaMeta:     mediaMeta,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	ms := drainMetrics(ch)
	if len(ms) < 2 {
		t.Errorf("collectSessions produced %d metrics, want at least 2", len(ms))
	}
}

func TestCollectSessionsEpisodeLabels(t *testing.T) {
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{"Player":{"device":"Roku"},"User":{"title":"viewer"}}`)
	mediaMeta := testMeta(t, `{
		"type":"episode",
		"grandparentTitle":"Breaking Bad",
		"parentTitle":"Season 1",
		"title":"Pilot"
	}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-3 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "TV Shows",
		LibID:       "2",
		LibType:     library.TypeShow,
		Meta:        meta,
		MediaMeta:   mediaMeta,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	ms := drainMetrics(ch)
	// 2 metrics: play_count, play_seconds
	if len(ms) != 2 {
		t.Errorf("collectSessions episode produced %d metrics, want 2", len(ms))
	}
}

func TestCollectSessionsMultipleSessions(t *testing.T) {
	tracker := sessions.NewTracker()
	meta1 := testMeta(t, `{
		"Player":{"device":"TV","local":true},
		"Session":{"location":"lan","bandwidth":8000},
		"User":{"title":"user1"},
		"Media":[{"bitrate":10000}]
	}`)
	meta2 := testMeta(t, `{
		"Player":{"device":"Phone"},
		"User":{"title":"user2"},
		"Media":[{"bitrate":3000}]
	}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-5 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "Movies",
		LibID:       "1",
		LibType:     library.TypeMovie,
		Meta:        meta1,
		MediaMeta:   testMeta(t, `{"type":"movie","title":"Movie A"}`),
	}
	tracker.Sessions["s2"] = sessions.Session{
		PlayStarted: time.Now().Add(-2 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "Movies",
		LibID:       "1",
		LibType:     library.TypeMovie,
		Meta:        meta2,
		MediaMeta:   testMeta(t, `{"type":"movie","title":"Movie B"}`),
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 30)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	ms := drainMetrics(ch)
	// 7 metrics:
	//   s1 has bandwidth=8000 and bitrate=10000 → play_count, play_seconds, session_bandwidth, session_bitrate (4)
	//   s2 has no bandwidth and bitrate=3000    → play_count, play_seconds, session_bitrate (3)
	if len(ms) != 7 {
		t.Errorf("collectSessions multi produced %d metrics, want 7", len(ms))
	}
}

func TestCollectSessions_stream_labels_from_media(t *testing.T) {
	// Verifies stream_type, stream_resolution, and stream_file_resolution
	// labels are populated from the live and file Media fields, and that the
	// plex_session_bitrate_kbps gauge matches Media[0].Bitrate (the bitrate
	// dimension that replaced the former stream_bitrate label).
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{
		"Player":{"device":"Chrome","product":"Plex Web","local":false},
		"Session":{"location":"wan"},
		"User":{"title":"testuser"},
		"Media":[{"videoResolution":"4k","bitrate":20000,"Part":[{"decision":"transcode","key":"/transcode/abc"}]}]
	}`)
	mediaMeta := testMeta(t, `{
		"type":"movie","title":"Stream Test",
		"Media":[{"videoResolution":"1080"}]
	}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:    time.Now().Add(-5 * time.Second),
		LastUpdate:     time.Now(),
		State:          sessions.StatePlaying,
		LibName:        "Movies",
		LibID:          "1",
		LibType:        library.TypeMovie,
		Meta:           meta,
		MediaMeta:      mediaMeta,
		TranscodeType:  "video",
		SubtitleAction: metrics.ValBurn,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(metrics.DescPlayCount)]
	if len(playMetrics) != 1 {
		t.Fatalf("expected 1 play_count metric, got %d", len(playMetrics))
	}

	labels, _ := metricSnapshot(t, playMetrics[0])

	// Verify stream labels derived from meta.Media
	if labels["stream_type"] != "transcode" {
		t.Errorf("stream_type = %q, want transcode", labels["stream_type"])
	}
	if labels["stream_resolution"] != "4k" {
		t.Errorf("stream_resolution = %q, want 4k", labels["stream_resolution"])
	}
	// stream_bitrate is no longer a label — now emitted as plex_session_bitrate_kbps gauge.
	if _, hasOldLabel := labels["stream_bitrate"]; hasOldLabel {
		t.Errorf("stream_bitrate label must not be present (migrated to plex_session_bitrate_kbps gauge)")
	}
	// Verify file resolution from mediaMeta.Media
	if labels["stream_file_resolution"] != "1080" {
		t.Errorf("stream_file_resolution = %q, want 1080", labels["stream_file_resolution"])
	}
	// Verify other labels
	if labels["transcode_type"] != "video" {
		t.Errorf("transcode_type = %q, want video", labels["transcode_type"])
	}
	if labels["subtitle_action"] != metrics.ValBurn {
		t.Errorf("subtitle_action = %q, want burn", labels["subtitle_action"])
	}
	if labels["local"] != "false" {
		t.Errorf("local = %q, want false", labels["local"])
	}
	if labels["location"] != "wan" {
		t.Errorf("location = %q, want wan", labels["location"])
	}

	// Verify plex_session_bitrate_kbps gauge value matches Media[0].Bitrate.
	bitrateMetrics := byDesc[descKey(metrics.DescSessionBitrate)]
	if len(bitrateMetrics) != 1 {
		t.Fatalf("expected 1 session_bitrate metric, got %d", len(bitrateMetrics))
	}
	_, brVal := metricSnapshot(t, bitrateMetrics[0])
	if brVal != 20000 {
		t.Errorf("session_bitrate value = %v, want 20000", brVal)
	}
}

func TestCollectSessions_no_media_uses_defaults(t *testing.T) {
	// When meta.Media is empty, stream_type should be "unknown" and
	// resolution empty. When mediaMeta.Media is empty, file_resolution empty.
	// No plex_session_bitrate_kbps sample is emitted for a zero-bitrate
	// session (prevents spurious series for fully-idle sessions).
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV","local":false},
		"Session":{},
		"User":{"title":"user1"}
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"No Media Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-1 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "Movies",
		LibID:       "1",
		LibType:     library.TypeMovie,
		Meta:        meta,
		MediaMeta:   mediaMeta,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(metrics.DescPlayCount)]
	if len(playMetrics) != 1 {
		t.Fatalf("expected 1 play_count metric, got %d", len(playMetrics))
	}

	labels, _ := metricSnapshot(t, playMetrics[0])

	if labels["stream_type"] != metrics.ValUnknown {
		t.Errorf("stream_type = %q, want %q (no media)", labels["stream_type"], metrics.ValUnknown)
	}
	if _, hasOldLabel := labels["stream_bitrate"]; hasOldLabel {
		t.Errorf("stream_bitrate label must not be present (migrated to plex_session_bitrate_kbps gauge)")
	}
	if labels["stream_resolution"] != "" {
		t.Errorf("stream_resolution = %q, want empty (no media)", labels["stream_resolution"])
	}
	if labels["stream_file_resolution"] != "" {
		t.Errorf("stream_file_resolution = %q, want empty (no mediaMeta media)", labels["stream_file_resolution"])
	}

	// With no Media, no bitrate sample should be emitted at all.
	if got := len(byDesc[descKey(metrics.DescSessionBitrate)]); got != 0 {
		t.Errorf("expected 0 session_bitrate metrics when Media is empty, got %d", got)
	}
}

func TestCollectSessions_local_true_label(t *testing.T) {
	// Verifies the local label is "true" when Player.Local is true.
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV","local":true},
		"User":{"title":"localuser"}
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Local Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-1 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "Movies",
		LibID:       "1",
		LibType:     library.TypeMovie,
		Meta:        meta,
		MediaMeta:   mediaMeta,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(metrics.DescPlayCount)]
	if len(playMetrics) != 1 {
		t.Fatalf("expected 1 play_count metric, got %d", len(playMetrics))
	}

	labels, _ := metricSnapshot(t, playMetrics[0])
	if labels["local"] != metrics.ValTrue {
		t.Errorf("local = %q, want true", labels["local"])
	}
}

func TestCollectSessions_library_lookup_sets_labels(t *testing.T) {
	// When a session has no libName it is resolved from the libs list;
	// when it already HAS a libName that value is kept, not overridden.
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user1"}}`)

	// Session WITH pre-set library labels — should keep them
	mediaMeta1 := testMeta(t, `{"type":"movie","title":"Movie A","librarySectionID":"99"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-1 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "PresetLib",
		LibID:       "42",
		LibType:     "movie",
		Meta:        meta,
		MediaMeta:   mediaMeta1,
	}

	// Session WITHOUT library labels — should resolve from libs
	mediaMeta2 := testMeta(t, `{"type":"movie","title":"Movie B","librarySectionID":"5"}`)
	tracker.Sessions["s2"] = sessions.Session{
		PlayStarted: time.Now().Add(-1 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		Meta:        meta,
		MediaMeta:   mediaMeta2,
	}

	libs := []library.Library{{ID: "5", Name: "4K Movies", Type: library.TypeMovie}}
	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 30)
	srv.collectSessions(ch, "Srv", "id1", libs)
	close(ch)

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(metrics.DescPlayCount)]
	if len(playMetrics) != 2 {
		t.Fatalf("expected 2 play_count metrics, got %d", len(playMetrics))
	}

	// Find each session's metric by the session label
	for _, m := range playMetrics {
		labels, _ := metricSnapshot(t, m)
		switch labels["session"] {
		case "s1":
			if labels["library"] != "PresetLib" {
				t.Errorf("s1 library = %q, want PresetLib (should keep preset)", labels["library"])
			}
			if labels["library_id"] != "42" {
				t.Errorf("s1 library_id = %q, want 42", labels["library_id"])
			}
		case "s2":
			if labels["library"] != "4K Movies" {
				t.Errorf("s2 library = %q, want 4K Movies (should resolve from libs)", labels["library"])
			}
			if labels["library_id"] != "5" {
				t.Errorf("s2 library_id = %q, want 5", labels["library_id"])
			}
		}
	}
}

func TestCollectSessions_bandwidth_only_when_positive(t *testing.T) {
	// Session with bandwidth=0 should NOT emit session_bandwidth metric.
	// Session with bandwidth>0 should emit with correct value.
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV"},
		"Session":{"bandwidth":0},
		"User":{"title":"user1"}
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"No BW Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-1 * time.Second),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		LibName:     "Movies",
		LibID:       "1",
		LibType:     library.TypeMovie,
		Meta:        meta,
		MediaMeta:   mediaMeta,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	bwMetrics := byDesc[descKey(metrics.DescSessionBandwidth)]
	if len(bwMetrics) != 0 {
		t.Errorf("expected 0 session_bandwidth metrics (bandwidth=0), got %d", len(bwMetrics))
	}
}

func TestCollectSessions_play_seconds_stopped_uses_prev(t *testing.T) {
	// A stopped session should use prevPlayedTime, not add time.Since(playStarted).
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user1"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Stopped Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:    time.Now().Add(-1000 * time.Second),
		LastUpdate:     time.Now(),
		State:          sessions.StateStopped,
		LibName:        "Movies",
		LibID:          "1",
		LibType:        library.TypeMovie,
		Meta:           meta,
		MediaMeta:      mediaMeta,
		PrevPlayedTime: 5 * time.Second,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	secMetrics := byDesc[descKey(metrics.DescPlaySeconds)]
	if len(secMetrics) != 1 {
		t.Fatalf("expected 1 play_seconds metric, got %d", len(secMetrics))
	}

	_, val := metricSnapshot(t, secMetrics[0])
	// Should be ~5 seconds (prevPlayedTime), NOT ~1000 seconds
	if val > 10 {
		t.Errorf("play_seconds = %v, want ~5 (stopped session should use prevPlayedTime only)", val)
	}
	if val < 4 {
		t.Errorf("play_seconds = %v, want ~5 (prevPlayedTime)", val)
	}
}
