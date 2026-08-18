package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/v2/internal/library"
	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"github.com/cplieger/plex-exporter/v2/internal/plextest"
	"github.com/cplieger/plex-exporter/v2/internal/sessions"
)

func TestRefreshSessions_basic_playing_session(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1",
				"ratingKey":"100",
				"type":"movie",
				"title":"Test Movie",
				"Player":{"device":"Chrome","product":"Plex Web","state":"playing","local":true},
				"Session":{"location":"lan","bandwidth":5000},
				"User":{"title":"testuser"},
				"Media":[{"videoResolution":"1080","bitrate":8000,"Part":[{"decision":"copy"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie",
				"title":"Test Movie",
				"librarySectionID":"1",
				"Media":[{"videoResolution":"1080"}]
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
	}

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	s, ok := snap["s1"]
	if !ok {
		t.Fatal("session 's1' not found in tracker")
	}
	if s.State != sessions.StatePlaying {
		t.Errorf("state = %q, want playing", s.State)
	}
	if s.LibName != "Movies" {
		t.Errorf("libName = %q, want Movies", s.LibName)
	}
	if s.LibID != "1" {
		t.Errorf("libID = %q, want 1", s.LibID)
	}
	srv.mu.Lock()
	reachable := srv.SessionsReachable
	srv.mu.Unlock()
	if !reachable {
		t.Errorf("sessionsReachable = %v, want true after successful poll", reachable)
	}
}

func TestRefreshSessions_with_transcode_session(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s2",
				"ratingKey":"200",
				"type":"movie",
				"title":"Transcode Movie",
				"Player":{"device":"Roku","product":"Plex for Roku","state":"playing","local":false},
				"Session":{"location":"wan","bandwidth":3000},
				"User":{"title":"viewer"},
				"Media":[{"videoResolution":"720","bitrate":4000,"Part":[{"decision":"transcode"}]}],
				"TranscodeSession":{
					"key":"/transcode/sessions/abc123",
					"videoDecision":"transcode",
					"audioDecision":"copy",
					"subtitleDecision":"burn",
					"sourceVideoCodec":"hevc",
					"sourceAudioCodec":"eac3",
					"videoCodec":"h264",
					"audioCodec":"eac3",
					"container":"mkv"
				}
			}]}}`)
		case "/library/metadata/200":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie",
				"title":"Transcode Movie",
				"librarySectionID":"1",
				"Media":[{"videoResolution":"1080"}]
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
	}

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	s, ok := snap["s2"]
	if !ok {
		t.Fatal("session 's2' not found in tracker")
	}
	if s.State != sessions.StatePlaying {
		t.Errorf("state = %q, want playing", s.State)
	}
	// TranscodeKind: videoDecision=transcode, audioDecision=copy,
	// sourceVideoCodec=hevc, videoCodec=h264 → video is transcoding.
	// audioDecision=copy, sourceAudioCodec=eac3, audioCodec=eac3 → no audio transcode.
	// Result: "video"
	if s.TranscodeType != metrics.ValVideo {
		t.Errorf("transcodeType = %q, want %q", s.TranscodeType, metrics.ValVideo)
	}
	// SubtitleAction: subtitleDecision=burn → "burn"
	if s.SubtitleAction != metrics.ValBurn {
		t.Errorf("subtitleAction = %q, want %q", s.SubtitleAction, metrics.ValBurn)
	}
}

func TestRefreshSessions_both_transcode(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s3",
				"ratingKey":"300",
				"type":"movie",
				"title":"Both Transcode",
				"Player":{"device":"TV","state":"playing"},
				"User":{"title":"u1"},
				"Media":[{"Part":[{"decision":"transcode"}]}],
				"TranscodeSession":{
					"key":"/transcode/sessions/def456",
					"videoDecision":"transcode",
					"audioDecision":"transcode",
					"subtitleDecision":"copy",
					"sourceVideoCodec":"hevc",
					"sourceAudioCodec":"dts",
					"videoCodec":"h264",
					"audioCodec":"aac",
					"container":"mkv"
				}
			}]}}`)
		case "/library/metadata/300":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Both Transcode"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	s := snap["s3"]
	if s.TranscodeType != metrics.ValBoth {
		t.Errorf("transcodeType = %q, want %q", s.TranscodeType, metrics.ValBoth)
	}
	if s.SubtitleAction != metrics.ValCopy {
		t.Errorf("subtitleAction = %q, want %q", s.SubtitleAction, metrics.ValCopy)
	}
}

func TestRefreshSessions_no_transcode_session(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s4",
				"ratingKey":"400",
				"type":"movie",
				"title":"Direct Play",
				"Player":{"device":"AppleTV","state":"playing","local":true},
				"User":{"title":"u2"},
				"Media":[{"videoResolution":"4k","Part":[{"decision":"directplay"}]}]
			}]}}`)
		case "/library/metadata/400":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Direct Play"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	s := snap["s4"]
	// No TranscodeSession → TranscodeType should remain default (empty)
	if s.TranscodeType != "" {
		t.Errorf("transcodeType = %q, want empty (no transcode session)", s.TranscodeType)
	}
	if s.SubtitleAction != "" {
		t.Errorf("subtitleAction = %q, want empty (no transcode session)", s.SubtitleAction)
	}
}

func TestRefreshSessions_empty_response(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	if len(snap) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(snap))
	}
	// A healthy "no one watching" poll must read reachable=1: the success
	// set is placed before the empty-sessions early return.
	srv.mu.Lock()
	reachable := srv.SessionsReachable
	srv.mu.Unlock()
	if !reachable {
		t.Errorf("sessionsReachable = %v, want true after successful empty poll", reachable)
	}
}

func TestRefreshSessions_invalid_rating_key_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s5",
				"ratingKey":"not-a-number",
				"Player":{"state":"playing"},
				"User":{"title":"u3"}
			}]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	if len(snap) != 0 {
		t.Errorf("expected 0 sessions (invalid rating key skipped), got %d", len(snap))
	}
	// Should have recorded an error
	srv.mu.Lock()
	errCount := srv.ErrorCounts["invalid_rating_key"]
	srv.mu.Unlock()
	if errCount != 1 {
		t.Errorf("errorCounts[invalid_rating_key] = %v, want 1", errCount)
	}
}

func TestRefreshSessions_fetch_error_records_error(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.SetSessionsReachable(true) // seed true so the error branch's flip to false is observable

	srv.RefreshSessions(t.Context())

	srv.mu.Lock()
	errCount := srv.ErrorCounts["sessions_fetch"]
	reachable := srv.SessionsReachable
	srv.mu.Unlock()
	if errCount != 1 {
		t.Errorf("errorCounts[sessions_fetch] = %v, want 1", errCount)
	}
	if reachable {
		t.Errorf("sessionsReachable = %v, want false after fetch error", reachable)
	}
}

func TestRefreshSessions_metadata_fetch_failure_still_updates_tracker(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s6",
				"ratingKey":"600",
				"type":"movie",
				"title":"Metadata Fail",
				"Player":{"device":"TV","state":"paused"},
				"User":{"title":"u4"},
				"Media":[{"Part":[{"decision":"copy"}]}]
			}]}}`)
		case "/library/metadata/600":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	srv.RefreshSessions(t.Context())

	// Session should still be tracked even if metadata fetch fails
	snap := srv.Sessions.SnapshotSessions()
	if len(snap) != 1 {
		t.Fatalf("expected 1 session, got %d", len(snap))
	}
	s := snap["s6"]
	if s.State != sessions.StatePaused {
		t.Errorf("state = %q, want paused", s.State)
	}
}

func TestRefreshSessions_multiple_sessions(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[
				{
					"sessionKey":"sa",
					"ratingKey":"10",
					"Player":{"state":"playing"},
					"User":{"title":"u1"},
					"Media":[{"Part":[{"decision":"directplay"}]}]
				},
				{
					"sessionKey":"sb",
					"ratingKey":"20",
					"Player":{"state":"paused"},
					"User":{"title":"u2"},
					"Media":[{"Part":[{"decision":"transcode"}]}],
					"TranscodeSession":{
						"key":"/transcode/sessions/xyz",
						"videoDecision":"copy",
						"audioDecision":"transcode",
						"subtitleDecision":"",
						"sourceVideoCodec":"h264",
						"sourceAudioCodec":"dts",
						"videoCodec":"h264",
						"audioCodec":"aac",
						"container":"mkv"
					}
				}
			]}}`)
		case "/library/metadata/10", "/library/metadata/20":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"type":"movie","title":"M"}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	srv.RefreshSessions(t.Context())

	snap := srv.Sessions.SnapshotSessions()
	if len(snap) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(snap))
	}
	if snap["sa"].State != sessions.StatePlaying {
		t.Errorf("sa state = %q, want playing", snap["sa"].State)
	}
	if snap["sb"].State != sessions.StatePaused {
		t.Errorf("sb state = %q, want paused", snap["sb"].State)
	}
	// sb has audio transcode (audioDecision=transcode, dts→aac)
	if snap["sb"].TranscodeType != metrics.ValAudio {
		t.Errorf("sb transcodeType = %q, want %q", snap["sb"].TranscodeType, metrics.ValAudio)
	}
}

func TestRunSessionPollLoop_cancels_cleanly(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { srv.RunSessionPollLoop(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunSessionPollLoop did not exit after context cancel")
	}
}

func TestFillSessionLibrary(t *testing.T) {
	libs := []library.Library{{ID: "5", Name: "4K Movies", Type: library.TypeMovie}}

	t.Run("preset libName is preserved", func(t *testing.T) {
		ss := &sessions.Session{LibName: "Preset", LibID: "1", LibType: "movie"}
		media := testMeta(t, `{"librarySectionID":"5"}`)
		fillSessionLibrary(ss, &media, libs)
		if ss.LibName != "Preset" || ss.LibID != "1" || ss.LibType != "movie" {
			t.Errorf("preset labels overwritten: got (%q,%q,%q)", ss.LibName, ss.LibID, ss.LibType)
		}
	})

	t.Run("matching section sets labels", func(t *testing.T) {
		ss := &sessions.Session{}
		media := testMeta(t, `{"librarySectionID":"5"}`)
		fillSessionLibrary(ss, &media, libs)
		if ss.LibName != "4K Movies" || ss.LibID != "5" || ss.LibType != library.TypeMovie {
			t.Errorf("labels = (%q,%q,%q), want (4K Movies,5,movie)", ss.LibName, ss.LibID, ss.LibType)
		}
	})

	t.Run("no matching section leaves labels empty", func(t *testing.T) {
		ss := &sessions.Session{}
		media := testMeta(t, `{"librarySectionID":"999"}`)
		fillSessionLibrary(ss, &media, libs)
		if ss.LibName != "" || ss.LibID != "" || ss.LibType != "" {
			t.Errorf("labels should be empty on no match: got (%q,%q,%q)", ss.LibName, ss.LibID, ss.LibType)
		}
	})
}

func TestRefreshSessions_vanished_session_marked_stopped(t *testing.T) {
	var pollCount int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			pollCount++
			if pollCount == 1 {
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"sessionKey":"s1","ratingKey":"100","type":"movie","Player":{"device":"TV","state":"playing"},"User":{"title":"u1"},"Media":[{"bitrate":8000,"Part":[{"decision":"copy"}]}]}]}}`)
				return
			}
			// Second poll: s1 has vanished from /status/sessions.
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"type":"movie","title":"Vanish Test","librarySectionID":"1"}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
	}

	// Poll 1: s1 is actively playing.
	srv.RefreshSessions(t.Context())
	snap := srv.Sessions.SnapshotSessions()
	if snap["s1"].State != sessions.StatePlaying {
		t.Fatalf("after poll 1: s1 state = %q, want playing", snap["s1"].State)
	}

	// Poll 2: s1 is absent, so RefreshSessions must reconcile the vanished
	// stream to StateStopped (the MarkAbsentStopped path) so it lands on the
	// 60s stopped-prune timer instead of lingering as playing until the 5m
	// stale timeout. It stays tracked because RefreshSessions does not prune.
	srv.RefreshSessions(t.Context())
	snap = srv.Sessions.SnapshotSessions()
	s, ok := snap["s1"]
	if !ok {
		t.Fatal("after poll 2: s1 should still be tracked (stopped, not yet pruned)")
	}
	if s.State != sessions.StateStopped {
		t.Errorf("after poll 2: s1 state = %q, want stopped (vanished session must be reconciled)", s.State)
	}
}

// TestRefreshSessions_metadata_cached_per_rating_key pins the per-key
// metadata cache: while a session stays on the same rating key the
// /library/metadata fetch happens exactly once (the tracked MediaMeta is
// reused), and a rating-key change (episode auto-advance within one
// session) invalidates the cache and refetches.
func TestRefreshSessions_metadata_cached_per_rating_key(t *testing.T) {
	var (
		mu        sync.Mutex
		metaHits  = map[string]int{}
		pollCount int
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			mu.Lock()
			pollCount++
			key := "100"
			if pollCount >= 3 {
				key = "101" // episode auto-advance: same session, new rating key
			}
			mu.Unlock()
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1",
				"ratingKey":"%s",
				"type":"episode",
				"Player":{"device":"TV","state":"playing"},
				"User":{"title":"u1"},
				"Media":[{"Part":[{"decision":"directplay"}]}]
			}]}}`, key)
		case "/library/metadata/100":
			mu.Lock()
			metaHits["100"]++
			mu.Unlock()
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"episode","title":"Episode One","librarySectionID":"1"
			}]}}`)
		case "/library/metadata/101":
			mu.Lock()
			metaHits["101"]++
			mu.Unlock()
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"episode","title":"Episode Two","librarySectionID":"1"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Shows", Type: library.TypeShow},
	}

	// Poll 1: first sight of s1/100 fetches metadata.
	srv.RefreshSessions(t.Context())
	mu.Lock()
	got := metaHits["100"]
	mu.Unlock()
	if got != 1 {
		t.Fatalf("after poll 1: metadata/100 hits = %d, want 1", got)
	}

	// Poll 2: same session, same rating key -- cached, no refetch.
	srv.RefreshSessions(t.Context())
	mu.Lock()
	got = metaHits["100"]
	mu.Unlock()
	if got != 1 {
		t.Errorf("after poll 2: metadata/100 hits = %d, want 1 (cached)", got)
	}
	snap := srv.Sessions.SnapshotSessions()
	if title := snap["s1"].MediaMeta.Title; title != "Episode One" {
		t.Errorf("after poll 2: MediaMeta.Title = %q, want Episode One (cache preserved)", title)
	}

	// Poll 3: rating key advances to 101 -- cache invalidated, refetch.
	srv.RefreshSessions(t.Context())
	mu.Lock()
	h100, h101 := metaHits["100"], metaHits["101"]
	mu.Unlock()
	if h100 != 1 || h101 != 1 {
		t.Errorf("after poll 3: metadata hits = 100:%d 101:%d, want 100:1 101:1", h100, h101)
	}
	snap = srv.Sessions.SnapshotSessions()
	if title := snap["s1"].MediaMeta.Title; title != "Episode Two" {
		t.Errorf("after poll 3: MediaMeta.Title = %q, want Episode Two (refetched on key change)", title)
	}
	if lib := snap["s1"].LibName; lib != "Shows" {
		t.Errorf("after poll 3: LibName = %q, want Shows", lib)
	}
}

// TestRefreshSessions_unresolved_library_refetches_metadata pins the
// degraded-start recovery contract: a session whose library labels could
// not resolve at first fetch (library list not yet known) keeps refetching
// metadata until the labels resolve, instead of caching the unresolved state
// for the session's lifetime.
func TestRefreshSessions_unresolved_library_refetches_metadata(t *testing.T) {
	var (
		mu       sync.Mutex
		metaHits int
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1",
				"ratingKey":"100",
				"type":"movie",
				"Player":{"device":"TV","state":"playing"},
				"User":{"title":"u1"},
				"Media":[{"Part":[{"decision":"directplay"}]}]
			}]}}`)
		case "/library/metadata/100":
			mu.Lock()
			metaHits++
			mu.Unlock()
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"M","librarySectionID":"1"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	// Degraded start: the library list is not known yet.

	srv.RefreshSessions(t.Context())
	snap := srv.Sessions.SnapshotSessions()
	if lib := snap["s1"].LibName; lib != "" {
		t.Fatalf("after poll 1: LibName = %q, want empty (no libraries known)", lib)
	}

	// Refresh recovered: the library list is now known. The next poll must
	// refetch (unresolved labels are not cached) and resolve the labels.
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
	}
	srv.RefreshSessions(t.Context())
	mu.Lock()
	got := metaHits
	mu.Unlock()
	if got != 2 {
		t.Errorf("metadata hits = %d, want 2 (unresolved labels force refetch)", got)
	}
	snap = srv.Sessions.SnapshotSessions()
	if lib := snap["s1"].LibName; lib != "Movies" {
		t.Errorf("after poll 2: LibName = %q, want Movies (labels resolved on refetch)", lib)
	}
}
