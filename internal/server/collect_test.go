package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"pgregory.net/rapid"
)

// testMeta constructs a plexapi.SessionMetadata from JSON to avoid anonymous
// struct tag mismatches in test literals.
func testMeta(t *testing.T, jsonStr string) plexapi.SessionMetadata {
	t.Helper()
	var m plexapi.SessionMetadata
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		t.Fatalf("testMeta: %v", err)
	}
	return m
}

// drainMetrics collects all metrics from a closed channel.
func drainMetrics(ch <-chan prometheus.Metric) []prometheus.Metric {
	var result []prometheus.Metric
	for m := range ch {
		result = append(result, m)
	}
	return result
}

// collectByDesc collects metrics from a channel and groups them by
// descriptor string.
func collectByDesc(ch <-chan prometheus.Metric) map[string][]prometheus.Metric {
	result := make(map[string][]prometheus.Metric)
	for m := range ch {
		desc := m.Desc().String()
		result[desc] = append(result[desc], m)
	}
	return result
}

// descKey returns the Desc().String() for a given prometheus.Desc for
// use as map key.
func descKey(d *prometheus.Desc) string { return d.String() }

// metricSnapshot extracts label pairs and the numeric value from a
// prometheus.Metric.
func metricSnapshot(t *testing.T, m prometheus.Metric) (labels map[string]string, value float64) {
	t.Helper()
	d := &dto.Metric{}
	if err := m.Write(d); err != nil {
		t.Fatalf("metricSnapshot: %v", err)
	}
	labels = make(map[string]string, len(d.GetLabel()))
	for _, lp := range d.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	if g := d.GetGauge(); g != nil {
		value = g.GetValue()
	} else if c := d.GetCounter(); c != nil {
		value = c.GetValue()
	}
	return labels, value
}

// --- Tests: sessionLabels ---

func TestSessionLabels(t *testing.T) {
	tests := []struct {
		name      string
		wantTitle string
		wantChild string
		wantGC    string
		meta      plexapi.SessionMetadata
	}{
		{
			name:      "movie",
			meta:      plexapi.SessionMetadata{Type: "movie", Title: "Inception"},
			wantTitle: "Inception",
		},
		{
			name: "episode",
			meta: plexapi.SessionMetadata{
				Type: "episode", GrandparentTitle: "Breaking Bad",
				ParentTitle: "Season 1", Title: "Pilot",
			},
			wantTitle: "Breaking Bad", wantChild: "Season 1", wantGC: "Pilot",
		},
		{
			name: "track",
			meta: plexapi.SessionMetadata{
				Type: "track", GrandparentTitle: "Pink Floyd",
				ParentTitle: "The Wall", Title: "Comfortably Numb",
			},
			wantTitle: "Pink Floyd", wantChild: "The Wall", wantGC: "Comfortably Numb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, child, gc := sessionLabels(&tt.meta)
			if title != tt.wantTitle || child != tt.wantChild || gc != tt.wantGC {
				t.Errorf("sessionLabels() = (%q, %q, %q), want (%q, %q, %q)",
					title, child, gc, tt.wantTitle, tt.wantChild, tt.wantGC)
			}
		})
	}
}

func TestSessionLabels_movie_returns_title_only(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		title := rapid.String().Draw(t, "title")
		m := &plexapi.SessionMetadata{Type: "movie", Title: title}
		gotTitle, gotChild, gotGC := sessionLabels(m)
		if gotTitle != title {
			t.Errorf("sessionLabels(movie) title = %q, want %q", gotTitle, title)
		}
		if gotChild != "" {
			t.Errorf("sessionLabels(movie) child = %q, want empty", gotChild)
		}
		if gotGC != "" {
			t.Errorf("sessionLabels(movie) grandchild = %q, want empty", gotGC)
		}
	})
}

func TestSessionLabels_episode_returns_hierarchy(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gp := rapid.String().Draw(t, "grandparent")
		p := rapid.String().Draw(t, "parent")
		title := rapid.String().Draw(t, "title")
		m := &plexapi.SessionMetadata{
			Type:             "episode",
			GrandparentTitle: gp,
			ParentTitle:      p,
			Title:            title,
		}
		gotTitle, gotChild, gotGC := sessionLabels(m)
		if gotTitle != gp {
			t.Errorf("sessionLabels(episode) title = %q, want %q", gotTitle, gp)
		}
		if gotChild != p {
			t.Errorf("sessionLabels(episode) child = %q, want %q", gotChild, p)
		}
		if gotGC != title {
			t.Errorf("sessionLabels(episode) grandchild = %q, want %q", gotGC, title)
		}
	})
}

// --- Tests: orDefault ---

func TestOrDefault(t *testing.T) {
	if got := orDefault("hello", "world"); got != "hello" {
		t.Errorf("orDefault(hello, world) = %q, want hello", got)
	}
	if got := orDefault("", "world"); got != "world" {
		t.Errorf("orDefault(empty, world) = %q, want world", got)
	}
}

func TestOrDefault_non_empty_returns_input(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringMatching(".+").Draw(t, "input")
		def := rapid.String().Draw(t, "default")
		got := orDefault(s, def)
		if got != s {
			t.Errorf("orDefault(%q, %q) = %q, want %q", s, def, got, s)
		}
	})
}

func TestOrDefault_empty_returns_default(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		def := rapid.String().Draw(t, "default")
		got := orDefault("", def)
		if got != def {
			t.Errorf("orDefault(\"\", %q) = %q, want %q", def, got, def)
		}
	})
}

// --- Tests: truncLabel (UTF-8 truncation at label boundary) ---

func TestTruncLabel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"short ascii passthrough", "hello", "hello"},
		{"exact max length passthrough", strings.Repeat("a", maxLabelLen), strings.Repeat("a", maxLabelLen)},
		{"one byte over max truncates", strings.Repeat("a", maxLabelLen+1), strings.Repeat("a", maxLabelLen)},
		// 127 ASCII + 2-byte é: boundary at byte 128 lands mid-é.
		{
			"2-byte codepoint straddling boundary is dropped",
			strings.Repeat("a", 127) + "é" + "xx",
			strings.Repeat("a", 127),
		},
		// 126 ASCII + 3-byte € (U+20AC): boundary at byte 128 lands mid-€.
		{
			"3-byte codepoint straddling boundary is dropped",
			strings.Repeat("a", 126) + "€" + "zz",
			strings.Repeat("a", 126),
		},
		// 125 ASCII + 4-byte 😀 (U+1F600): boundary at byte 128 lands mid-emoji.
		{
			"4-byte codepoint straddling boundary is dropped",
			strings.Repeat("a", 125) + "😀" + "zz",
			strings.Repeat("a", 125),
		},
		// 126 ASCII + 2-byte é ends at byte 128: keep all 128.
		{
			"codepoint ending exactly at boundary is kept",
			strings.Repeat("a", 126) + "é" + "zz",
			strings.Repeat("a", 126) + "é",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncLabel(tc.input)
			if got != tc.want {
				t.Errorf("truncLabel(%q) = %q (len=%d), want %q (len=%d)",
					tc.input, got, len(got), tc.want, len(tc.want))
			}
		})
	}
}

func TestTruncLabel_invariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "input")
		got := truncLabel(s)

		if len(got) > maxLabelLen {
			t.Fatalf("truncLabel(%q) len=%d exceeds maxLabelLen=%d", s, len(got), maxLabelLen)
		}
		if len(got) > len(s) {
			t.Fatalf("truncLabel(%q) len=%d exceeds input len=%d", s, len(got), len(s))
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("truncLabel(%q) produced invalid UTF-8 %q from valid input", s, got)
		}
		// Idempotent: truncating again must return the same value.
		if got2 := truncLabel(got); got2 != got {
			t.Fatalf("truncLabel not idempotent: truncLabel(%q)=%q, truncLabel(%q)=%q",
				s, got, got, got2)
		}
	})
}

// --- Tests: streamLabels ---

func TestStreamLabels(t *testing.T) {
	tests := []struct {
		name     string
		wantType string
		wantRes  string
		meta     plexapi.SessionMetadata
	}{
		{
			name:     "no media",
			meta:     plexapi.SessionMetadata{},
			wantType: metrics.ValUnknown,
			wantRes:  "",
		},
		{
			name: "media with part decision",
			meta: plexapi.SessionMetadata{
				Media: []plexapi.MediaInfo{{
					VideoResolution: "1080",
					Part:            []plexapi.MediaPart{{Decision: "transcode"}},
				}},
			},
			wantType: "transcode",
			wantRes:  "1080",
		},
		{
			name: "media with empty part decision",
			meta: plexapi.SessionMetadata{
				Media: []plexapi.MediaInfo{{
					VideoResolution: "4k",
					Part:            []plexapi.MediaPart{{Decision: ""}},
				}},
			},
			wantType: metrics.ValUnknown,
			wantRes:  "4k",
		},
		{
			name: "media with no parts",
			meta: plexapi.SessionMetadata{
				Media: []plexapi.MediaInfo{{VideoResolution: "720"}},
			},
			wantType: metrics.ValUnknown,
			wantRes:  "720",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotRes := streamLabels(&tt.meta)
			if gotType != tt.wantType {
				t.Errorf("streamLabels() type = %q, want %q", gotType, tt.wantType)
			}
			if gotRes != tt.wantRes {
				t.Errorf("streamLabels() res = %q, want %q", gotRes, tt.wantRes)
			}
		})
	}
}

// --- Tests: resolveLibrary ---

func TestResolveLibrary(t *testing.T) {
	libs := map[string]library.Library{
		"5": {ID: "5", Name: "4K Movies", Type: library.TypeMovie},
	}
	tests := []struct {
		name     string
		wantName string
		wantID   string
		wantType string
		sess     sessions.Session
	}{
		{
			name:     "cached library labels",
			sess:     sessions.Session{LibName: "Movies", LibID: "1", LibType: library.TypeMovie},
			wantName: "Movies",
			wantID:   "1",
			wantType: library.TypeMovie,
		},
		{
			name: "lookup by librarySectionID",
			sess: sessions.Session{
				MediaMeta: plexapi.SessionMetadata{LibrarySectionID: "5"},
			},
			wantName: "4K Movies",
			wantID:   "5",
			wantType: library.TypeMovie,
		},
		{
			name: "unknown fallback",
			sess: sessions.Session{
				MediaMeta: plexapi.SessionMetadata{LibrarySectionID: "999"},
			},
			wantName: metrics.ValUnknown,
			wantID:   "0",
			wantType: metrics.ValUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, id, typ := resolveLibrary(&tt.sess, libs)
			if name != tt.wantName {
				t.Errorf("resolveLibrary() name = %q, want %q", name, tt.wantName)
			}
			if id != tt.wantID {
				t.Errorf("resolveLibrary() id = %q, want %q", id, tt.wantID)
			}
			if typ != tt.wantType {
				t.Errorf("resolveLibrary() type = %q, want %q", typ, tt.wantType)
			}
		})
	}
}

// --- Tests: LabelAllowlist.Normalize ---

func TestNormalizeLabel(t *testing.T) {
	allowed := &metrics.LabelAllowlist{
		Name:     "test",
		Allowed:  map[string]bool{"movie": true, "episode": true},
		Fallback: "other",
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"known value passthrough", "movie", "movie"},
		{"case insensitive match returns lowercased", "Movie", "movie"},
		{"unknown value returns other", "clip", "other"},
		{"empty string returns other", "", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allowed.Normalize(tt.input)
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeLabel_with_known_allowlists(t *testing.T) {
	// Verify that the actual production allowlists work as expected.
	tests := []struct {
		name  string
		input string
		list  *metrics.LabelAllowlist
		want  string
	}{
		{"known stream type", "transcode", metrics.StreamTypeAllowlist, "transcode"},
		{"unknown stream type", "remux", metrics.StreamTypeAllowlist, "other"},
		{"known media type", "episode", metrics.MediaTypeAllowlist, "episode"},
		{"unknown media type", "audiobook", metrics.MediaTypeAllowlist, "other"},
		{"known resolution", "1080", metrics.ResolutionAllowlist, "1080"},
		{"unknown resolution", "8k", metrics.ResolutionAllowlist, "other"},
		{"empty resolution is known", "", metrics.ResolutionAllowlist, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.list.Normalize(tt.input)
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	srv := &Server{Sessions: sessions.NewTracker()}
	ch := make(chan *prometheus.Desc, len(metrics.AllDescs)*2)
	srv.Describe(ch)
	close(ch)

	got := make(map[*prometheus.Desc]bool)
	for d := range ch {
		got[d] = true
	}
	for _, want := range metrics.AllDescs {
		if !got[want] {
			t.Errorf("Describe missing %s", want.String())
		}
	}
	if len(got) != len(metrics.AllDescs) {
		t.Errorf("Describe sent %d unique descriptors, want %d", len(got), len(metrics.AllDescs))
	}
}

func TestCollectServerMetrics(t *testing.T) {
	srv := &Server{
		Name:             "TestServer",
		ID:               "abc123",
		Version:          "1.40.0",
		Platform:         "Linux",
		PlatformVersion:  "6.1",
		PlexPass:         true,
		HostCPU:          0.42,
		HostMem:          0.65,
		TransmitBytes:    12345,
		ActiveTranscodes: 2,
		Libraries: []library.Library{
			{ID: "1", Name: "Movies", Type: library.TypeMovie, DurationTotal: 1000, StorageTotal: 2000, ItemsCount: 50},
			{ID: "2", Name: "TV Shows", Type: library.TypeShow, DurationTotal: 3000, StorageTotal: 4000, ItemsCount: 0},
		},
		Sessions: sessions.NewTracker(),
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	ms := drainMetrics(ch)
	// Base: server_info, cpu, mem, transmit, active_transcodes,
	// http_reachable, session_poll_reachable, http_retries + len(metrics.ErrorTypes) error counters.
	// Plus: 2x lib_duration, 2x lib_storage, 1x lib_items (only Movies has count>0), est_transmit
	want := 8 + len(metrics.ErrorTypes) + 5 + 1
	if len(ms) != want {
		t.Errorf("Collect produced %d metrics, want %d", len(ms), want)
		for i, m := range ms {
			t.Logf("  [%d] %s", i, m.Desc().String())
		}
	}
}

func TestCollectWithPlexPassFalse(t *testing.T) {
	srv := &Server{
		Name:     "Srv",
		ID:       "id1",
		PlexPass: false,
		Sessions: sessions.NewTracker(),
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	ms := drainMetrics(ch)
	// Base: server_info, cpu, mem, transmit, active_transcodes,
	// http_reachable, session_poll_reachable, http_retries + len(metrics.ErrorTypes) error counters, est_transmit.
	want := 8 + len(metrics.ErrorTypes) + 1
	if len(ms) != want {
		t.Errorf("Collect produced %d metrics, want %d", len(ms), want)
	}
}

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
	// 5 metrics: play_count, play_seconds, session_bandwidth, session_bitrate, est_transmit
	if len(ms) != 5 {
		t.Errorf("collectSessions produced %d metrics, want 5", len(ms))
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
	// Only est_transmit(1) — session skipped due to zero playStarted
	if len(ms) != 1 {
		t.Errorf("collectSessions produced %d metrics, want 1 (est_transmit only)", len(ms))
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
	// 3 metrics: play_count, play_seconds, est_transmit
	if len(ms) != 3 {
		t.Errorf("collectSessions produced %d metrics, want 3", len(ms))
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
	// 3 metrics: play_count, play_seconds, est_transmit
	if len(ms) != 3 {
		t.Errorf("collectSessions produced %d metrics, want 3", len(ms))
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

func TestCollectSessionsEstimatedTransmitAccumulates(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.TotalEstimatedKBits = 1000

	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user4"},"Media":[{"bitrate":5000}]}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Bitrate Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-10 * time.Second),
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

	ms := drainMetrics(ch)
	if len(ms) < 3 {
		t.Errorf("collectSessions produced %d metrics, want at least 3", len(ms))
	}
}

func TestCollectWithActiveSessions(t *testing.T) {
	tracker := sessions.NewTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV","product":"Plex for LG","local":true},
		"Session":{"location":"lan","bandwidth":10000},
		"User":{"title":"admin"},
		"Media":[{"videoResolution":"2160","bitrate":20000,"Part":[{"decision":"transcode"}]}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"4K Movie","Media":[{"videoResolution":"2160"}]}`)
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

	srv := &Server{
		Name:             "TestPlex",
		ID:               "plex1",
		Version:          "1.40.0",
		Platform:         "Linux",
		PlatformVersion:  "6.1",
		PlexPass:         true,
		HostCPU:          0.5,
		HostMem:          0.7,
		TransmitBytes:    50000,
		ActiveTranscodes: 1,
		Libraries: []library.Library{
			{ID: "1", Name: "Movies", Type: library.TypeMovie, DurationTotal: 5000, StorageTotal: 10000, ItemsCount: 100},
		},
		Sessions: tracker,
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	ms := drainMetrics(ch)
	// Base 8 (+ metrics.ErrorTypes) + 1 lib_duration + 1 lib_storage
	// + 1 lib_items + play_count + play_seconds + session_bandwidth + session_bitrate + est_transmit.
	want := 8 + len(metrics.ErrorTypes) + 3 + 5
	if len(ms) != want {
		t.Errorf("Collect with sessions produced %d metrics, want %d", len(ms), want)
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
	// 3 metrics: play_count, play_seconds, est_transmit
	if len(ms) != 3 {
		t.Errorf("collectSessions episode produced %d metrics, want 3", len(ms))
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
	// 8 metrics:
	//   s1 has bandwidth=8000 and bitrate=10000 → play_count, play_seconds, session_bandwidth, session_bitrate (4)
	//   s2 has no bandwidth and bitrate=3000    → play_count, play_seconds, session_bitrate (3)
	//   plus est_transmit (1)
	if len(ms) != 8 {
		t.Errorf("collectSessions multi produced %d metrics, want 8", len(ms))
	}
}

func TestCollectMultipleLibraries(t *testing.T) {
	srv := &Server{
		Name:    "Srv",
		ID:      "id1",
		Version: "1.0",
		Libraries: []library.Library{
			{ID: "1", Name: "Movies", Type: library.TypeMovie, DurationTotal: 100, StorageTotal: 200, ItemsCount: 10},
			{ID: "2", Name: "TV", Type: library.TypeShow, DurationTotal: 300, StorageTotal: 400, ItemsCount: 20},
			{ID: "3", Name: "Music", Type: library.TypeArtist, DurationTotal: 500, StorageTotal: 600, ItemsCount: 30},
		},
		Sessions: sessions.NewTracker(),
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	ms := drainMetrics(ch)
	// Base 8 (+ metrics.ErrorTypes) + 3x lib_duration, 3x lib_storage,
	// 3x lib_items, + est_transmit.
	want := 8 + len(metrics.ErrorTypes) + 9 + 1
	if len(ms) != want {
		t.Errorf("Collect multi-lib produced %d metrics, want %d", len(ms), want)
	}
}

func TestCollectSessions_stream_labels_from_media(t *testing.T) {
	// Targets lived mutants at lines 850 (len(sess.Meta.Media) > 0)
	// and 860 (len(sess.MediaMeta.Media) > 0).
	// Verifies that stream_type, stream_resolution, and
	// stream_file_resolution labels are correctly populated from Media fields.
	// Also verifies the new plex_session_bitrate_kbps gauge value matches the
	// Media[0].Bitrate field (replaces the former stream_bitrate label).
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
	// This is the inverse of the above test — catches negation of the > 0 checks.
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
	// Targets lived mutant at line 892 (sess.Meta.Player.Local negation).
	// Verifies local="true" when Player.Local is true.
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
	// Targets lived mutant at line 870 (libName == "" negation).
	// When session has no libName, it should be resolved from the libs list.
	// When session HAS a libName, it should NOT be overridden.
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

func TestCollectSessions_estimated_transmit_multiplication_factor(t *testing.T) {
	// Targets lived mutants at lines 911 (arithmetic in elapsed*bitrate)
	// and 915 (total*128 multiplication).
	// With no active sessions and totalEstimatedKBits=1000,
	// est_transmit should be 1000*128 = 128000.
	tracker := sessions.NewTracker()
	tracker.TotalEstimatedKBits = 1000

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 10)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(metrics.DescEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	// 1000 kbits * 128 = 128000 bytes
	if value != 128000 {
		t.Errorf("est_transmit = %v, want 128000 (1000 * 128)", value)
	}
}

func TestCollectSessions_estimated_transmit_zero_when_empty(t *testing.T) {
	// With no sessions and zero totalEstimatedKBits, est_transmit should be 0.
	tracker := sessions.NewTracker()

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 10)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(metrics.DescEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	if value != 0 {
		t.Errorf("est_transmit = %v, want 0", value)
	}
}

func TestCollectSessions_stopped_session_no_additional_estimated(t *testing.T) {
	// Targets lived mutant at line 910 (sess.State == sessions.StatePlaying negation).
	// A stopped session should NOT add time.Since(playStarted)*bitrate to estimated.
	// Only prevPlayedTime contributes via totalEstimatedKBits (already accumulated on stop).
	tracker := sessions.NewTracker()
	tracker.TotalEstimatedKBits = 500

	meta := testMeta(t, `{
		"Player":{"device":"TV"},
		"User":{"title":"user1"},
		"Media":[{"bitrate":10000}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Stopped Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:    time.Now().Add(-100 * time.Second),
		LastUpdate:     time.Now(),
		State:          sessions.StateStopped,
		LibName:        "Movies",
		LibID:          "1",
		LibType:        library.TypeMovie,
		Meta:           meta,
		MediaMeta:      mediaMeta,
		PrevPlayedTime: 10 * time.Second,
	}

	srv := &Server{Name: "Srv", ID: "id1", Sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(metrics.DescEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	// Only totalEstimatedKBits (500) contributes. Stopped session does NOT add
	// time.Since(playStarted)*bitrate. So value = 500 * 128 = 64000.
	if value != 64000 {
		t.Errorf("est_transmit = %v, want 64000 (stopped session should not add elapsed*bitrate)", value)
	}
}

func TestCollectSessions_playing_session_adds_estimated(t *testing.T) {
	// A playing session SHOULD add time.Since(playStarted)*bitrate to estimated.
	// This is the complement of the stopped test above.
	tracker := sessions.NewTracker()
	tracker.TotalEstimatedKBits = 0

	meta := testMeta(t, `{
		"Player":{"device":"TV"},
		"User":{"title":"user1"},
		"Media":[{"bitrate":10000}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Playing Movie"}`)
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now().Add(-10 * time.Second),
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
	estMetrics := byDesc[descKey(metrics.DescEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	// ~10 seconds * 10000 kbits * 128 bytes/kbit ≈ 12,800,000
	// Allow some tolerance for timing
	if value < 1000000 {
		t.Errorf("est_transmit = %v, want > 1000000 (playing session should add elapsed*bitrate)", value)
	}
}

func TestCollect_host_metrics_values(t *testing.T) {
	// Verify actual numeric values of host CPU and memory metrics.
	srv := &Server{
		Name:     "Srv",
		ID:       "id1",
		HostCPU:  0.42,
		HostMem:  0.65,
		Sessions: sessions.NewTracker(),
	}

	ch := make(chan prometheus.Metric, 20)
	srv.Collect(ch)
	close(ch)

	byDesc := collectByDesc(ch)

	cpuMetrics := byDesc[descKey(metrics.DescHostCPU)]
	if len(cpuMetrics) != 1 {
		t.Fatalf("expected 1 cpu metric, got %d", len(cpuMetrics))
	}
	_, cpuVal := metricSnapshot(t, cpuMetrics[0])
	if cpuVal != 0.42 {
		t.Errorf("host_cpu = %v, want 0.42", cpuVal)
	}

	memMetrics := byDesc[descKey(metrics.DescHostMem)]
	if len(memMetrics) != 1 {
		t.Fatalf("expected 1 mem metric, got %d", len(memMetrics))
	}
	_, memVal := metricSnapshot(t, memMetrics[0])
	if memVal != 0.65 {
		t.Errorf("host_mem = %v, want 0.65", memVal)
	}
}

func TestCollect_library_items_only_when_positive(t *testing.T) {
	// Libraries with ItemsCount=0 should NOT emit lib_items metric.
	// Libraries with ItemsCount>0 should emit with correct content_type label.
	srv := &Server{
		Name: "Srv",
		ID:   "id1",
		Libraries: []library.Library{
			{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 100},
			{ID: "2", Name: "TV", Type: library.TypeShow, ItemsCount: 0},
		},
		Sessions: sessions.NewTracker(),
	}

	ch := make(chan prometheus.Metric, 30)
	srv.Collect(ch)
	close(ch)

	byDesc := collectByDesc(ch)
	itemMetrics := byDesc[descKey(metrics.DescLibItems)]
	if len(itemMetrics) != 1 {
		t.Fatalf("expected 1 lib_items metric (only Movies), got %d", len(itemMetrics))
	}

	labels, val := metricSnapshot(t, itemMetrics[0])
	if val != 100 {
		t.Errorf("lib_items value = %v, want 100", val)
	}
	if labels["content_type"] != "movies" {
		t.Errorf("content_type = %q, want movies", labels["content_type"])
	}
	if labels["library"] != "Movies" {
		t.Errorf("library = %q, want Movies", labels["library"])
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

// TestSessionIndex pins the grandchild_index label derivation: episodes and
// tracks expose their Plex index (episode/track number) as a string; movies
// and other types, and a missing/zero index, yield "".
func TestSessionIndex(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"episode with index", `{"type":"episode","index":5}`, "5"},
		{"track with index", `{"type":"track","index":12}`, "12"},
		{"movie has no episode number", `{"type":"movie","index":0}`, ""},
		{"episode missing index", `{"type":"episode"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testMeta(t, tc.json)
			if got := sessionIndex(&m); got != tc.want {
				t.Errorf("sessionIndex(%s) = %q, want %q", tc.json, got, tc.want)
			}
		})
	}
}
