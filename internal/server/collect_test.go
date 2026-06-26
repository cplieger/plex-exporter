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

// TestTruncLabel_all_continuation_bytes_walks_to_zero forces truncLabel's
// UTF-8 boundary-walk loop all the way down to i==0. A string longer than
// maxLabelLen made entirely of continuation bytes (0x80) has no rune-start
// byte, so the loop decrements i from maxLabelLen down to 0. The guard must
// stop AT zero (i > 0): a >= guard would index s[0], step to i=-1, and panic
// on s[:-1]. Plex API label strings are user-controlled and may be malformed
// UTF-8, so this is a real input class.
func TestTruncLabel_all_continuation_bytes_walks_to_zero(t *testing.T) {
	input := strings.Repeat("\x80", maxLabelLen+1)

	got := truncLabel(input)

	if got != "" {
		t.Errorf("truncLabel(all-continuation-bytes len=%d) = %q (len=%d), want %q",
			len(input), got, len(got), "")
	}
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
