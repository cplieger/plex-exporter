package wsclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"pgregory.net/rapid"
)

func TestTranscodeKind(t *testing.T) {
	tests := []struct {
		name string
		ts   plexapi.WSTranscodeSession
		want string
	}{
		{
			name: "video only",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "transcode", AudioDecision: "copy"},
			want: "video",
		},
		{
			name: "audio only",
			ts:   plexapi.WSTranscodeSession{AudioDecision: "transcode", VideoDecision: "copy"},
			want: "audio",
		},
		{
			name: "both",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "transcode", AudioDecision: "transcode"},
			want: "both",
		},
		{
			name: "none direct play",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "copy", AudioDecision: "copy"},
			want: metrics.ValNone,
		},
		{
			name: "codec change implies video transcode",
			ts:   plexapi.WSTranscodeSession{SourceVideoCodec: "hevc", VideoCodec: "h264"},
			want: "video",
		},
		{
			name: "codec change implies audio transcode",
			ts:   plexapi.WSTranscodeSession{SourceAudioCodec: "truehd", AudioCodec: "aac"},
			want: "audio",
		},
		{
			name: "same codecs no transcode",
			ts:   plexapi.WSTranscodeSession{SourceVideoCodec: "h264", VideoCodec: "h264", SourceAudioCodec: "aac", AudioCodec: "aac"},
			want: metrics.ValNone,
		},
		{
			name: "whitespace trimmed",
			ts:   plexapi.WSTranscodeSession{VideoDecision: " transcode ", AudioDecision: " copy "},
			want: "video",
		},
		{
			name: "empty session",
			ts:   plexapi.WSTranscodeSession{},
			want: metrics.ValNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TranscodeKind(&tt.ts)
			if got != tt.want {
				t.Errorf("TranscodeKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubtitleAction(t *testing.T) {
	tests := []struct {
		name string
		ts   plexapi.WSTranscodeSession
		want string
	}{
		{name: "burn", ts: plexapi.WSTranscodeSession{SubtitleDecision: "burn"}, want: metrics.ValBurn},
		{name: "burn-in", ts: plexapi.WSTranscodeSession{SubtitleDecision: "burn-in"}, want: metrics.ValBurn},
		{name: "copy", ts: plexapi.WSTranscodeSession{SubtitleDecision: "copy"}, want: metrics.ValCopy},
		{name: "copying", ts: plexapi.WSTranscodeSession{SubtitleDecision: "copying"}, want: metrics.ValCopy},
		{name: "transcode", ts: plexapi.WSTranscodeSession{SubtitleDecision: "transcode"}, want: metrics.ValTranscode},
		{name: "transcoding", ts: plexapi.WSTranscodeSession{SubtitleDecision: "transcoding"}, want: metrics.ValTranscode},
		{
			name: "empty with srt container implies copy",
			ts:   plexapi.WSTranscodeSession{Container: "srt"},
			want: metrics.ValCopy,
		},
		{
			name: "empty with video transcode implies burn",
			ts:   plexapi.WSTranscodeSession{VideoDecision: "transcode"},
			want: metrics.ValBurn,
		},
		{
			name: "empty no transcode",
			ts:   plexapi.WSTranscodeSession{},
			want: metrics.ValNone,
		},
		{
			name: "unknown value passed through",
			ts:   plexapi.WSTranscodeSession{SubtitleDecision: "embed"},
			want: "other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SubtitleAction(&tt.ts)
			if got != tt.want {
				t.Errorf("SubtitleAction() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTranscodeKind_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &plexapi.WSTranscodeSession{
			VideoDecision:    rapid.String().Draw(t, "videoDecision"),
			AudioDecision:    rapid.String().Draw(t, "audioDecision"),
			SourceVideoCodec: rapid.String().Draw(t, "srcVideo"),
			SourceAudioCodec: rapid.String().Draw(t, "srcAudio"),
			VideoCodec:       rapid.String().Draw(t, "videoCodec"),
			AudioCodec:       rapid.String().Draw(t, "audioCodec"),
		}
		got := TranscodeKind(ts)
		valid := map[string]bool{"video": true, "audio": true, "both": true, metrics.ValNone: true}
		if !valid[got] {
			t.Errorf("TranscodeKind() = %q, not in valid set", got)
		}
	})
}

func TestSubtitleAction_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &plexapi.WSTranscodeSession{
			SubtitleDecision: rapid.String().Draw(t, "subtitleDecision"),
			Container:        rapid.String().Draw(t, "container"),
			VideoDecision:    rapid.String().Draw(t, "videoDecision"),
		}
		got := SubtitleAction(ts)
		if got == "" {
			t.Error("SubtitleAction() returned empty string")
		}
	})
}

func TestTranscodeKind_video_decision_dominates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &plexapi.WSTranscodeSession{
			VideoDecision: "transcode",
			AudioDecision: rapid.SampledFrom([]string{"copy", "direct play", ""}).Draw(t, "audio"),
		}
		got := TranscodeKind(ts)
		if got != "video" && got != "both" {
			t.Errorf("TranscodeKind() with video=transcode = %q, want video or both", got)
		}
	})
}

func TestTranscodeKind_audio_decision_dominates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &plexapi.WSTranscodeSession{
			VideoDecision: rapid.SampledFrom([]string{"copy", "direct play", ""}).Draw(t, "video"),
			AudioDecision: "transcode",
		}
		got := TranscodeKind(ts)
		if got != "audio" && got != "both" {
			t.Errorf("TranscodeKind() with audio=transcode = %q, want audio or both", got)
		}
	})
}

func TestSubtitleAction_srt_in_container_string(t *testing.T) {
	ts := &plexapi.WSTranscodeSession{Container: "mkv-srt-embedded"}
	got := SubtitleAction(ts)
	if got != metrics.ValCopy {
		t.Errorf("SubtitleAction(container=mkv-srt-embedded) = %q, want copy", got)
	}
}

func TestSubtitleAction_whitespace_trimmed(t *testing.T) {
	ts := &plexapi.WSTranscodeSession{SubtitleDecision: " burn "}
	got := SubtitleAction(ts)
	if got != metrics.ValBurn {
		t.Errorf("SubtitleAction(SubtitleDecision=' burn ') = %q, want burn", got)
	}
}

func TestSubtitleAction_case_insensitive(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"BURN", metrics.ValBurn},
		{"Burn-In", metrics.ValBurn},
		{"COPY", metrics.ValCopy},
		{"Copying", metrics.ValCopy},
		{"TRANSCODE", metrics.ValTranscode},
		{"Transcoding", metrics.ValTranscode},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ts := &plexapi.WSTranscodeSession{SubtitleDecision: tt.input}
			got := SubtitleAction(ts)
			if got != tt.want {
				t.Errorf("SubtitleAction(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTranscodeKind_mixed_codec_change_and_decision(t *testing.T) {
	ts := &plexapi.WSTranscodeSession{
		VideoDecision:    "transcode",
		SourceAudioCodec: "truehd",
		AudioCodec:       "aac",
	}
	got := TranscodeKind(ts)
	if got != "both" {
		t.Errorf("TranscodeKind(video=transcode + audio codec change) = %q, want both", got)
	}
}

func TestTranscodeKind_empty_codec_no_transcode(t *testing.T) {
	ts := &plexapi.WSTranscodeSession{
		SourceVideoCodec: "h264",
		VideoCodec:       "",
	}
	got := TranscodeKind(ts)
	if got != metrics.ValNone {
		t.Errorf("TranscodeKind(empty VideoCodec) = %q, want none", got)
	}
}

func TestFillSessionLibrary_already_set_noop(t *testing.T) {
	ss := &sessions.Session{LibName: "Existing", LibID: "42", LibType: library.TypeMovie}
	media := &plexapi.SessionMetadata{LibrarySectionID: "1"}
	libs := []library.Library{{ID: "1", Name: "Movies", Type: library.TypeMovie}}

	FillSessionLibrary(ss, media, libs)

	if ss.LibName != "Existing" {
		t.Errorf("libName = %q, want Existing (should not be overwritten)", ss.LibName)
	}
	if ss.LibID != "42" {
		t.Errorf("libID = %q, want 42", ss.LibID)
	}
}

func TestFillSessionLibrary_resolves_from_libs(t *testing.T) {
	ss := &sessions.Session{}
	media := &plexapi.SessionMetadata{LibrarySectionID: "3"}
	libs := []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
		{ID: "3", Name: "TV Shows", Type: library.TypeShow},
	}

	FillSessionLibrary(ss, media, libs)

	if ss.LibName != "TV Shows" {
		t.Errorf("libName = %q, want TV Shows", ss.LibName)
	}
	if ss.LibID != "3" {
		t.Errorf("libID = %q, want 3", ss.LibID)
	}
	if ss.LibType != library.TypeShow {
		t.Errorf("libType = %q, want show", ss.LibType)
	}
}

func TestFillSessionLibrary_no_match_leaves_empty(t *testing.T) {
	ss := &sessions.Session{}
	media := &plexapi.SessionMetadata{LibrarySectionID: "999"}
	libs := []library.Library{{ID: "1", Name: "Movies", Type: library.TypeMovie}}

	FillSessionLibrary(ss, media, libs)

	if ss.LibName != "" {
		t.Errorf("libName = %q, want empty (no match)", ss.LibName)
	}
}

func TestMarkTranscodePending(t *testing.T) {
	tests := []struct {
		name             string
		transcodeSession string
		existingType     string
		existingKey      string
		wantType         string
		wantKey          string
	}{
		{
			name:             "sets pending and stores key when transcode session present and type empty",
			transcodeSession: "tc123",
			existingType:     "",
			wantType:         metrics.ValPending,
			wantKey:          "tc123",
		},
		{
			name:             "noop when transcode session empty",
			transcodeSession: "",
			existingType:     "",
			wantType:         "",
			wantKey:          "",
		},
		{
			name:             "noop when type already set; key not overwritten",
			transcodeSession: "tc123",
			existingType:     "video",
			existingKey:      "prev",
			wantType:         "video",
			wantKey:          "prev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ss := &sessions.Session{TranscodeType: tt.existingType, TranscodeKey: tt.existingKey}
			MarkTranscodePending(ss, tt.transcodeSession)
			if ss.TranscodeType != tt.wantType {
				t.Errorf("TranscodeType = %q, want %q", ss.TranscodeType, tt.wantType)
			}
			if ss.TranscodeKey != tt.wantKey {
				t.Errorf("TranscodeKey = %q, want %q", ss.TranscodeKey, tt.wantKey)
			}
		})
	}
}

func TestHandleTranscodeUpdateMatchesPending(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:   time.Now(),
		LastUpdate:    time.Now(),
		State:         sessions.StatePlaying,
		TranscodeType: metrics.ValPending,
		TranscodeKey:  "tc1",
	}

	l := listenerFor(nil, tracker, nil)

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.Type = "transcodeSession.update"
	notif.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:           "/transcode/sessions/tc1",
		VideoDecision: "transcode",
		AudioDecision: "copy",
	}}

	l.HandleTranscodeUpdate(notif)

	snap, _ := tracker.SnapshotSessions()
	s := snap["s1"]

	if s.TranscodeType != "video" {
		t.Errorf("transcodeType = %q, want video", s.TranscodeType)
	}
	// subtitleAction: empty SubtitleDecision + VideoDecision="transcode" → "burn"
	if s.SubtitleAction != metrics.ValBurn {
		t.Errorf("subtitleAction = %q, want burn", s.SubtitleAction)
	}
}

func TestHandleTranscodeUpdateMatchesByPartKey(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now(),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		Meta: plexapi.SessionMetadata{
			Media: []plexapi.MediaInfo{{
				Part: []plexapi.MediaPart{{Key: "/transcode/sessions/tc42/progress"}},
			}},
		},
	}

	l := listenerFor(nil, tracker, nil)

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:              "tc42",
		VideoDecision:    "transcode",
		AudioDecision:    "transcode",
		SubtitleDecision: "burn",
	}}

	l.HandleTranscodeUpdate(notif)

	snap, _ := tracker.SnapshotSessions()
	s := snap["s1"]

	if s.TranscodeType != "both" {
		t.Errorf("transcodeType = %q, want both", s.TranscodeType)
	}
	if s.SubtitleAction != metrics.ValBurn {
		t.Errorf("subtitleAction = %q, want burn", s.SubtitleAction)
	}
}

func TestHandleTranscodeUpdateNoMatch(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:   time.Now(),
		LastUpdate:    time.Now(),
		State:         sessions.StatePlaying,
		TranscodeType: "video",
	}

	l := listenerFor(nil, tracker, nil)

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:           "tc_unknown",
		VideoDecision: "transcode",
	}}

	l.HandleTranscodeUpdate(notif)

	snap, _ := tracker.SnapshotSessions()
	s := snap["s1"]

	// Should remain unchanged since no match
	if s.TranscodeType != "video" {
		t.Errorf("transcodeType = %q, want video (unchanged)", s.TranscodeType)
	}
}

func TestHandleTranscodeUpdateMultipleSessions(t *testing.T) {
	// Reproduces the cross-session-misattribution scenario described in
	// plex-exporter full-review finding h-f5. Two sessions are simultaneously
	// pending transcode classification. Each playing notification carries its
	// own bare transcode key on the session (populated by markTranscodePending);
	// transcodeSession.update events arrive keyed as
	// /transcode/sessions/<bare-id>. The matcher must correlate by the bare
	// suffix, not by the pending boolean, or a transcode update can be applied
	// to the wrong session depending on Go map iteration order.
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:   time.Now(),
		LastUpdate:    time.Now(),
		State:         sessions.StatePlaying,
		TranscodeType: metrics.ValPending,
		TranscodeKey:  "tc_s1",
	}
	tracker.Sessions["s2"] = sessions.Session{
		PlayStarted:   time.Now(),
		LastUpdate:    time.Now(),
		State:         sessions.StatePlaying,
		TranscodeType: metrics.ValPending,
		TranscodeKey:  "tc_s2",
	}

	l := listenerFor(nil, tracker, nil)

	// Transcode update for s2 arrives first; must route to s2 regardless of
	// map iteration order, not to s1.
	notif := plexapi.WSNotification{}
	notif.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:           "/transcode/sessions/tc_s2",
		VideoDecision: "transcode",
		AudioDecision: "copy",
	}}
	l.HandleTranscodeUpdate(notif)

	snap, _ := tracker.SnapshotSessions()
	s1 := snap["s1"]
	s2 := snap["s2"]

	if s1.TranscodeType != metrics.ValPending {
		t.Errorf("s1 should remain pending (not misattributed), got %q", s1.TranscodeType)
	}
	if s2.TranscodeType != "video" {
		t.Errorf("s2 transcodeType = %q, want video", s2.TranscodeType)
	}

	// Update for s1 now arrives; must route to s1, leaving s2 intact.
	notif2 := plexapi.WSNotification{}
	notif2.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:           "/transcode/sessions/tc_s1",
		AudioDecision: "transcode",
	}}
	l.HandleTranscodeUpdate(notif2)

	snap, _ = tracker.SnapshotSessions()
	s1 = snap["s1"]
	s2 = snap["s2"]

	if s1.TranscodeType != "audio" {
		t.Errorf("s1 transcodeType = %q, want audio", s1.TranscodeType)
	}
	if s2.TranscodeType != "video" {
		t.Errorf("s2 transcodeType disturbed = %q, want video", s2.TranscodeType)
	}
}

func TestHandleTranscodeUpdate_fallback_by_part_key(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted: time.Now(),
		LastUpdate:  time.Now(),
		State:       sessions.StatePlaying,
		Meta: plexapi.SessionMetadata{
			Media: []plexapi.MediaInfo{{
				Part: []plexapi.MediaPart{{Key: "/transcode/sessions/tc99/data"}},
			}},
		},
	}

	l := listenerFor(nil, tracker, nil)

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:           "tc99",
		VideoDecision: "transcode",
	}}
	l.HandleTranscodeUpdate(notif)

	snap, _ := tracker.SnapshotSessions()
	s1 := snap["s1"]

	if s1.TranscodeType != "video" {
		t.Errorf("s1 transcodeType = %q, want video (part-key fallback)", s1.TranscodeType)
	}
}

func TestHandleTranscodeUpdate_pending_without_key_is_no_longer_catchall(t *testing.T) {
	tracker := sessions.NewTracker()
	tracker.Sessions["s1"] = sessions.Session{
		PlayStarted:   time.Now(),
		LastUpdate:    time.Now(),
		State:         sessions.StatePlaying,
		TranscodeType: metrics.ValPending, // pending but NO transcodeKey stored yet
	}

	l := listenerFor(nil, tracker, nil)

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.TranscodeSession = []plexapi.WSTranscodeSession{{
		Key:           "/transcode/sessions/unrelated",
		VideoDecision: "transcode",
	}}
	l.HandleTranscodeUpdate(notif)

	snap, _ := tracker.SnapshotSessions()
	s1 := snap["s1"]

	if s1.TranscodeType != metrics.ValPending {
		t.Errorf("pending session without stored key was misattributed: transcodeType = %q, want pending",
			s1.TranscodeType)
	}
}

func TestHandlePlaying_updates_session_from_api(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"Chrome","product":"Plex Web","state":"playing","local":true},
				"Session":{"location":"lan","bandwidth":5000},
				"User":{"title":"testuser","id":"1"},
				"Media":[{"videoResolution":"1080","bitrate":8000,"Part":[{"decision":"copy"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Test Movie",
				"librarySectionID":"1",
				"Media":[{"videoResolution":"1080"}]
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()
	libs := []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
	}

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.Type = "playing"
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	listenerFor(client, tracker, libs).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	s, ok := snap["s1"]

	if !ok {
		t.Fatal("session s1 not found")
	}
	if s.State != sessions.StatePlaying {
		t.Errorf("state = %q, want playing", s.State)
	}
	if s.MediaMeta.Title != "Test Movie" {
		t.Errorf("mediaMeta.Title = %q, want Test Movie", s.MediaMeta.Title)
	}
	if s.LibName != "Movies" {
		t.Errorf("libName = %q, want Movies", s.LibName)
	}
	if s.LibID != "1" {
		t.Errorf("libID = %q, want 1", s.LibID)
	}
}

func TestHandlePlaying_stopped_updates_without_api(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	// Pre-populate a playing session
	meta := &plexapi.SessionMetadata{Title: "Playing Movie", Media: []plexapi.MediaInfo{{Bitrate: 5000}}}
	tracker.Update("s1", sessions.StatePlaying, meta, nil)

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "stopped",
	}}

	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	s := snap["s1"]

	if s.State != sessions.StateStopped {
		t.Errorf("state = %q, want stopped", s.State)
	}
}

func TestHandlePlaying_invalid_rating_key_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"abc",
				"Player":{"device":"TV"},
				"User":{"title":"user1"}
			}]}}`)
			return
		}
		t.Errorf("unexpected request to %s (should not fetch metadata for invalid key)", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "abc",
		State:      "playing",
	}}

	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	_, ok := snap["s1"]

	if ok {
		t.Error("session should not be created for invalid rating key")
	}
}

func TestHandlePlaying_session_not_in_api_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			// Return sessions but not the one we're looking for
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"other","ratingKey":"200",
				"Player":{"device":"TV"},
				"User":{"title":"user2"}
			}]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	_, ok := snap["s1"]

	if ok {
		t.Error("session should not be created when not found in sessions API")
	}
}

func TestHandlePlaying_empty_metadata_response_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"}
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	_, ok := snap["s1"]

	if ok {
		t.Error("session should not be created when metadata response is empty")
	}
}

func TestHandlePlaying_transcode_session_marks_pending(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"},
				"Media":[{"videoResolution":"1080","Part":[{"decision":"transcode"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Transcoded Movie","librarySectionID":"1"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey:       "s1",
		RatingKey:        "100",
		State:            "playing",
		TranscodeSession: "tc123",
	}}

	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	s := snap["s1"]

	if s.TranscodeType != metrics.ValPending {
		t.Errorf("transcodeType = %q, want pending", s.TranscodeType)
	}
	if s.TranscodeKey != "tc123" {
		t.Errorf("transcodeKey = %q, want tc123 (bare id stored for later correlation)", s.TranscodeKey)
	}
}

func TestHandlePlaying_sessions_api_failure_returns_early(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	// Should not panic, just return early
	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	count := len(snap)

	if count != 0 {
		t.Errorf("sessions count = %d, want 0 (API failure)", count)
	}
}

func TestHandlePlaying_metadata_fetch_error_skips_session(t *testing.T) {
	// Targets uncovered line 1066-1068: when metadata fetch fails for a valid rating key.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"}
			}]}}`)
		case "/library/metadata/100":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	listenerFor(client, tracker, nil).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	_, ok := snap["s1"]

	if ok {
		t.Error("session should not be created when metadata fetch fails")
	}
}

func TestHandlePlaying_library_not_found_uses_unknown(t *testing.T) {
	// Targets uncovered line 1096-1097: when library ID doesn't match any known library.
	// The session should still be created but with unknown library labels.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"},
				"Media":[{"videoResolution":"1080","Part":[{"decision":"copy"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Orphan Movie","librarySectionID":"999"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	tracker := sessions.NewTracker()
	// Only library ID "1" exists — session has librarySectionID "999"
	libs := []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
	}

	notif := plexapi.WSNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []plexapi.WSPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	listenerFor(client, tracker, libs).HandlePlaying(context.Background(), notif)

	snap, _ := tracker.SnapshotSessions()
	s, ok := snap["s1"]

	if !ok {
		t.Fatal("session s1 should be created")
	}
	// Library labels should remain empty (not resolved) since ID 999 doesn't match
	if s.LibName != "" {
		t.Errorf("libName = %q, want empty (library not found)", s.LibName)
	}
}
