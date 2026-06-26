package sessions

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
)

func TestTrackerUpdate(t *testing.T) {
	tracker := NewTracker()

	meta := &plexapi.SessionMetadata{Title: "Test Movie", Type: "movie"}
	tracker.Update("s1", StatePlaying, meta, nil)

	tracker.mu.Lock()
	s := tracker.Sessions["s1"]
	tracker.mu.Unlock()

	if s.State != StatePlaying {
		t.Errorf("state = %q, want playing", s.State)
	}
	if s.Meta.Title != "Test Movie" {
		t.Errorf("title = %q, want Test Movie", s.Meta.Title)
	}
	if s.PlayStarted.IsZero() {
		t.Error("PlayStarted should be set")
	}
}

func TestTrackerUpdate_stop_accumulates_time(t *testing.T) {
	tracker := NewTracker()

	meta := &plexapi.SessionMetadata{
		Title: "Test",
		Media: []plexapi.MediaInfo{{Bitrate: 1000}},
	}
	tracker.Update("s1", StatePlaying, meta, nil)
	time.Sleep(10 * time.Millisecond)
	tracker.Update("s1", StateStopped, nil, nil)

	tracker.mu.Lock()
	s := tracker.Sessions["s1"]
	tracker.mu.Unlock()

	if s.PrevPlayedTime == 0 {
		t.Error("PrevPlayedTime should be > 0 after stop")
	}
	if tracker.TotalEstimatedKBits == 0 {
		t.Error("TotalEstimatedKBits should be > 0")
	}
}

func TestTrackerUpdateMetadata(t *testing.T) {
	tracker := NewTracker()

	meta := &plexapi.SessionMetadata{Title: "Original"}
	tracker.Update("s1", StatePlaying, meta, nil)

	newMeta := &plexapi.SessionMetadata{Title: "Updated"}
	tracker.Update("s1", StatePlaying, newMeta, nil)

	tracker.mu.Lock()
	s := tracker.Sessions["s1"]
	tracker.mu.Unlock()

	if s.Meta.Title != "Updated" {
		t.Errorf("title = %q, want Updated", s.Meta.Title)
	}
}

func TestTrackerNilMeta(t *testing.T) {
	tracker := NewTracker()

	tracker.Update("s1", StatePlaying, nil, nil)

	tracker.mu.Lock()
	s := tracker.Sessions["s1"]
	tracker.mu.Unlock()

	if s.State != StatePlaying {
		t.Errorf("state = %q, want playing", s.State)
	}
}

func TestRunPruneLoopCancellation(t *testing.T) {
	tracker := NewTracker()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		tracker.RunPruneLoop(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// ok — loop exited on cancelled context
	case <-time.After(time.Second):
		t.Fatal("RunPruneLoop did not exit on cancelled context")
	}
}

func TestSessionTrackerResumeAfterStop(t *testing.T) {
	tracker := NewTracker()

	meta := &plexapi.SessionMetadata{
		Title: "Resume Test",
		Media: []plexapi.MediaInfo{{Bitrate: 2000}},
	}
	tracker.Update("s1", StatePlaying, meta, nil)
	time.Sleep(10 * time.Millisecond)
	tracker.Update("s1", StateStopped, nil, nil)

	tracker.mu.Lock()
	prev := tracker.Sessions["s1"].PrevPlayedTime
	tracker.mu.Unlock()

	// Resume playing
	tracker.Update("s1", StatePlaying, nil, nil)
	time.Sleep(10 * time.Millisecond)
	tracker.Update("s1", StateStopped, nil, nil)

	tracker.mu.Lock()
	after := tracker.Sessions["s1"].PrevPlayedTime
	tracker.mu.Unlock()

	if after <= prev {
		t.Errorf("prevPlayedTime should accumulate: before=%v, after=%v", prev, after)
	}
}

func TestSessionTrackerMediaMetaUpdate(t *testing.T) {
	tracker := NewTracker()

	meta := &plexapi.SessionMetadata{Title: "Session"}
	mediaMeta := &plexapi.SessionMetadata{Title: "Media Info", Type: "movie"}
	tracker.Update("s1", StatePlaying, meta, mediaMeta)

	tracker.mu.Lock()
	s := tracker.Sessions["s1"]
	tracker.mu.Unlock()

	if s.MediaMeta.Title != "Media Info" {
		t.Errorf("mediaMeta.Title = %q, want Media Info", s.MediaMeta.Title)
	}
	if s.MediaMeta.Type != "movie" {
		t.Errorf("mediaMeta.Type = %q, want movie", s.MediaMeta.Type)
	}
}

func TestSessionTrackerUpdate_truncates_long_session_key(t *testing.T) {
	tracker := NewTracker()
	longKey := strings.Repeat("x", MaxSessionKeyLen+20)
	meta := &plexapi.SessionMetadata{Title: "Long Key"}
	tracker.Update(longKey, StatePlaying, meta, nil)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	truncated := longKey[:MaxSessionKeyLen]
	if _, ok := tracker.Sessions[truncated]; !ok {
		t.Errorf("session should be stored under truncated key (len=%d)", MaxSessionKeyLen)
	}
	if _, ok := tracker.Sessions[longKey]; ok {
		t.Error("session should NOT be stored under the full-length key")
	}
}

func TestSessionTrackerUpdate_rejects_new_when_full(t *testing.T) {
	tracker := NewTracker()

	// Fill to capacity
	for i := range MaxTrackedSessions {
		tracker.Update(fmt.Sprintf("s%d", i), StatePlaying, nil, nil)
	}

	tracker.mu.Lock()
	count := len(tracker.Sessions)
	tracker.mu.Unlock()

	if count != MaxTrackedSessions {
		t.Fatalf("sessions count = %d, want %d", count, MaxTrackedSessions)
	}

	// New session should be rejected
	tracker.Update("new-session", StatePlaying, nil, nil)

	tracker.mu.Lock()
	_, exists := tracker.Sessions["new-session"]
	afterCount := len(tracker.Sessions)
	tracker.mu.Unlock()

	if exists {
		t.Error("new session should be rejected when tracker is full")
	}
	if afterCount != MaxTrackedSessions {
		t.Errorf("sessions count = %d, want %d (unchanged)", afterCount, MaxTrackedSessions)
	}
}

func TestSessionTrackerUpdate_existing_session_updates_when_full(t *testing.T) {
	tracker := NewTracker()

	// Fill to capacity
	for i := range MaxTrackedSessions {
		tracker.Update(fmt.Sprintf("s%d", i), StatePlaying, nil, nil)
	}

	// Existing session should still update
	tracker.Update("s0", StateStopped, nil, nil)

	tracker.mu.Lock()
	s := tracker.Sessions["s0"]
	tracker.mu.Unlock()

	if s.State != StateStopped {
		t.Errorf("existing session state = %q, want stopped (should update even when full)", s.State)
	}
}

func TestSessionTrackerUpdate_stop_accumulates_estimated_kbits(t *testing.T) {
	// Stopping a playing session that carries media accumulates
	// TotalEstimatedKBits from the elapsed play time and the media bitrate.
	tracker := NewTracker()

	meta := &plexapi.SessionMetadata{
		Title: "Test",
		Media: []plexapi.MediaInfo{{Bitrate: 1000}},
	}
	// Manually set up a playing session with a known playStarted time
	tracker.mu.Lock()
	tracker.Sessions["s1"] = Session{
		State:       StatePlaying,
		PlayStarted: time.Now().Add(-10 * time.Second),
		LastUpdate:  time.Now(),
		Meta:        *meta,
	}
	tracker.mu.Unlock()

	tracker.Update("s1", StateStopped, nil, nil)

	tracker.mu.Lock()
	kbits := tracker.TotalEstimatedKBits
	tracker.mu.Unlock()

	// Should be ~10s * 1000 bitrate = ~10000 kbits
	if kbits < 5000 {
		t.Errorf("totalEstimatedKBits = %v, want > 5000 after stop", kbits)
	}
	if kbits > 20000 {
		t.Errorf("totalEstimatedKBits = %v, unexpectedly large", kbits)
	}
}

func TestSessionTrackerUpdate_stop_without_media_no_accumulation(t *testing.T) {
	// When session has no media, stopping should NOT accumulate estimated kbits.
	tracker := NewTracker()

	// Manually set up a playing session without media
	tracker.mu.Lock()
	tracker.Sessions["s1"] = Session{
		State:       StatePlaying,
		PlayStarted: time.Now().Add(-10 * time.Second),
		LastUpdate:  time.Now(),
		Meta:        plexapi.SessionMetadata{Title: "No Media"},
	}
	tracker.mu.Unlock()

	tracker.Update("s1", StateStopped, nil, nil)

	tracker.mu.Lock()
	kbits := tracker.TotalEstimatedKBits
	tracker.mu.Unlock()

	if kbits != 0 {
		t.Errorf("totalEstimatedKBits = %v, want 0 (no media)", kbits)
	}
}

func TestUpdateLibraryLabels_normalizes_long_key(t *testing.T) {
	// Regression: UpdateLibraryLabels must truncate the key the same way
	// Update does, so that a session stored via Update with a >64-byte key
	// is found by UpdateLibraryLabels using the original long key.
	tracker := NewTracker()
	longKey := strings.Repeat("a", MaxSessionKeyLen+30)

	// Store session via Update (internally truncates).
	tracker.Update(longKey, StatePlaying, &plexapi.SessionMetadata{Title: "LongKey"}, nil)

	// Verify stored under truncated key.
	truncated := longKey[:MaxSessionKeyLen]
	tracker.mu.Lock()
	if _, ok := tracker.Sessions[truncated]; !ok {
		t.Fatal("session not found under truncated key after Update")
	}
	tracker.mu.Unlock()

	// UpdateLibraryLabels with the SAME original long key must resolve.
	var called bool
	tracker.UpdateLibraryLabels(longKey, func(s *Session) {
		called = true
		s.LibName = "Movies"
		s.LibID = "1"
		s.LibType = "movie"
	})
	if !called {
		t.Fatal("UpdateLibraryLabels did not find session with long key (truncation mismatch)")
	}

	tracker.mu.Lock()
	s := tracker.Sessions[truncated]
	tracker.mu.Unlock()
	if s.LibName != "Movies" {
		t.Errorf("LibName = %q, want Movies", s.LibName)
	}
}

func TestUpdateLibraryLabels_short_key_unchanged(t *testing.T) {
	// Verify that short keys (<=MaxSessionKeyLen) still work as before.
	tracker := NewTracker()
	shortKey := "short-session-id"

	tracker.Update(shortKey, StatePlaying, &plexapi.SessionMetadata{Title: "Short"}, nil)

	var called bool
	tracker.UpdateLibraryLabels(shortKey, func(s *Session) {
		called = true
		s.LibName = "TV Shows"
	})
	if !called {
		t.Fatal("UpdateLibraryLabels should find session with short key")
	}

	tracker.mu.Lock()
	s := tracker.Sessions[shortKey]
	tracker.mu.Unlock()
	if s.LibName != "TV Shows" {
		t.Errorf("LibName = %q, want TV Shows", s.LibName)
	}
}

// snapshotTranscodeIndex returns a copy of the tracker's transcodeIndex under
// the tracker lock so tests can assert on the exact set of
// (transcodeKey -> sessionKey) entries. Always non-nil so an empty result
// compares equal to an empty map literal.
func snapshotTranscodeIndex(t *testing.T, tr *Tracker) map[string]string {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make(map[string]string, len(tr.transcodeIndex))
	maps.Copy(out, tr.transcodeIndex)
	return out
}

// TestUpdateLibraryLabels_transcodeIndexWriteGate pins the two-operand guard
//
//	if ss.TranscodeKey != "" && ss.TranscodeKey != oldKey { ... write index ... }
//
// in UpdateLibraryLabels by asserting the exact transcodeIndex state after the
// call, across inputs where each operand changes the outcome. The index entry
// is written only when the post-fn key is non-empty AND differs from the
// pre-fn key.
func TestUpdateLibraryLabels_transcodeIndexWriteGate(t *testing.T) {
	const id = "sess-1"

	cases := []struct {
		wantIndex map[string]string
		name      string
		preKey    string
		postKey   string
	}{
		{
			name:      "new non-empty key writes index entry",
			preKey:    "",
			postKey:   "tk-new",
			wantIndex: map[string]string{"tk-new": id},
		},
		{
			name:      "unchanged non-empty key writes nothing",
			preKey:    "tk-x",
			postKey:   "tk-x",
			wantIndex: map[string]string{},
		},
		{
			name:      "cleared key writes nothing",
			preKey:    "tk-y",
			postKey:   "",
			wantIndex: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tr.mu.Lock()
			tr.Sessions[id] = Session{TranscodeKey: tc.preKey}
			tr.mu.Unlock()

			tr.UpdateLibraryLabels(id, func(s *Session) {
				s.TranscodeKey = tc.postKey
			})

			got := snapshotTranscodeIndex(t, tr)
			if !maps.Equal(got, tc.wantIndex) {
				t.Errorf("UpdateLibraryLabels(preKey=%q -> postKey=%q): transcodeIndex = %v, want %v",
					tc.preKey, tc.postKey, got, tc.wantIndex)
			}
		})
	}
}

func TestNormalizeKey(t *testing.T) {
	// normalizeKey clamps a session key to MaxSessionKeyLen so writes and
	// lookups always use the same map key. Three regions matter: below the cap
	// (unchanged), exactly at the cap (unchanged — the limit is inclusive), and
	// above the cap (truncated to exactly MaxSessionKeyLen bytes).
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short key unchanged", "abc123", "abc123"},
		{"below cap unchanged", strings.Repeat("a", MaxSessionKeyLen-1), strings.Repeat("a", MaxSessionKeyLen-1)},
		{"at cap unchanged", strings.Repeat("b", MaxSessionKeyLen), strings.Repeat("b", MaxSessionKeyLen)},
		{"one past cap truncated", strings.Repeat("c", MaxSessionKeyLen+1), strings.Repeat("c", MaxSessionKeyLen)},
		{"well past cap truncated", strings.Repeat("y", MaxSessionKeyLen+50), strings.Repeat("y", MaxSessionKeyLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeKey(tc.in); got != tc.want {
				t.Errorf("normalizeKey(len=%d) = %q (len %d), want len %d",
					len(tc.in), got, len(got), len(tc.want))
			}
		})
	}
}

// TestUpdateTranscode covers the three correlation paths and the no-match
// case. The transcode index is the fast path; the fallback scan checks each
// session's stored TranscodeKey (and back-populates the index on a hit) before
// scanning media Part URLs.
func TestUpdateTranscode(t *testing.T) {
	t.Run("index fast path matches and sets classification", func(t *testing.T) {
		tr := NewTracker()
		tr.mu.Lock()
		tr.Sessions["sess1"] = Session{State: StatePlaying}
		tr.transcodeIndex["bare-key"] = "sess1"
		tr.mu.Unlock()

		if !tr.UpdateTranscode("/transcode/bare-key/0", metrics.ValVideo, metrics.ValBurn) {
			t.Fatal("UpdateTranscode should match via the transcode index")
		}

		tr.mu.Lock()
		defer tr.mu.Unlock()
		s := tr.Sessions["sess1"]
		if s.TranscodeType != metrics.ValVideo {
			t.Errorf("TranscodeType = %q, want %q", s.TranscodeType, metrics.ValVideo)
		}
		if s.SubtitleAction != metrics.ValBurn {
			t.Errorf("SubtitleAction = %q, want %q", s.SubtitleAction, metrics.ValBurn)
		}
	})

	t.Run("fallback by stored TranscodeKey back-populates the index", func(t *testing.T) {
		tr := NewTracker()
		tr.mu.Lock()
		tr.Sessions["sess1"] = Session{State: StatePlaying, TranscodeKey: "tk-1"}
		tr.mu.Unlock()

		if !tr.UpdateTranscode("prefix/tk-1/suffix", metrics.ValBoth, metrics.ValCopy) {
			t.Fatal("UpdateTranscode should match via the stored TranscodeKey")
		}

		tr.mu.Lock()
		defer tr.mu.Unlock()
		s := tr.Sessions["sess1"]
		if s.TranscodeType != metrics.ValBoth || s.SubtitleAction != metrics.ValCopy {
			t.Errorf("classification = (%q, %q), want (%q, %q)",
				s.TranscodeType, s.SubtitleAction, metrics.ValBoth, metrics.ValCopy)
		}
		if got := tr.transcodeIndex["tk-1"]; got != "sess1" {
			t.Errorf("transcodeIndex[tk-1] = %q, want sess1 (a TranscodeKey hit must back-populate the index)", got)
		}
	})

	t.Run("fallback by media part key matches without touching the index", func(t *testing.T) {
		tr := NewTracker()
		tr.mu.Lock()
		tr.Sessions["sess1"] = Session{
			State: StatePlaying,
			Meta: plexapi.SessionMetadata{
				Media: []plexapi.MediaInfo{{Part: []plexapi.MediaPart{{Key: "/library/parts/77/file.mkv?key=xyz"}}}},
			},
		}
		tr.mu.Unlock()

		if !tr.UpdateTranscode("key=xyz", metrics.ValAudio, metrics.ValNone) {
			t.Fatal("UpdateTranscode should match when a media part key contains the transcode key")
		}

		tr.mu.Lock()
		defer tr.mu.Unlock()
		if s := tr.Sessions["sess1"]; s.TranscodeType != metrics.ValAudio {
			t.Errorf("TranscodeType = %q, want %q", s.TranscodeType, metrics.ValAudio)
		}
		if len(tr.transcodeIndex) != 0 {
			t.Errorf("transcodeIndex = %v, want empty (a part-key match must not populate the index)", tr.transcodeIndex)
		}
	})

	t.Run("no match returns false and mutates nothing", func(t *testing.T) {
		tr := NewTracker()
		tr.mu.Lock()
		tr.Sessions["sess1"] = Session{State: StatePlaying, TranscodeKey: "tk-1", TranscodeType: metrics.ValNone}
		tr.mu.Unlock()

		if tr.UpdateTranscode("no-such-key", metrics.ValVideo, metrics.ValBurn) {
			t.Fatal("UpdateTranscode should return false when nothing matches")
		}

		tr.mu.Lock()
		defer tr.mu.Unlock()
		s := tr.Sessions["sess1"]
		if s.TranscodeType != metrics.ValNone || s.SubtitleAction != "" {
			t.Errorf("session mutated on no-match: TranscodeType=%q SubtitleAction=%q", s.TranscodeType, s.SubtitleAction)
		}
	})
}
