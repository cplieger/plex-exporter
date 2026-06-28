package sessions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestParseState(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want State
	}{
		{"playing", "playing", StatePlaying},
		{"stopped", "stopped", StateStopped},
		{"paused", "paused", StatePaused},
		{"unknown maps to other", "buffering", StateOther},
		{"empty maps to other", "", StateOther},
		{"case-sensitive Playing is not playing", "Playing", StateOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseState(tc.raw); got != tc.want {
				t.Errorf("ParseState(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestTrackerUpdate_playing_to_paused_accumulates(t *testing.T) {
	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["s1"] = Session{
		State:       StatePlaying,
		PlayStarted: time.Now().Add(-10 * time.Second),
		LastUpdate:  time.Now(),
		Meta:        plexapi.SessionMetadata{Media: []plexapi.MediaInfo{{Bitrate: 1000}}},
	}
	tracker.mu.Unlock()

	tracker.Update("s1", StatePaused, nil, nil)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	s := tracker.Sessions["s1"]
	if s.State != StatePaused {
		t.Errorf("state = %q, want paused", s.State)
	}
	if s.PrevPlayedTime == 0 {
		t.Error("PrevPlayedTime should accumulate on a playing->paused transition")
	}
}

func TestTrackerUpdate_paused_to_playing_resets_playstarted(t *testing.T) {
	tracker := NewTracker()
	oldStart := time.Now().Add(-time.Hour)
	tracker.mu.Lock()
	tracker.Sessions["s1"] = Session{
		State:       StatePaused,
		PlayStarted: oldStart,
		LastUpdate:  time.Now(),
	}
	tracker.mu.Unlock()

	tracker.Update("s1", StatePlaying, nil, nil)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	s := tracker.Sessions["s1"]
	if !s.PlayStarted.After(oldStart) {
		t.Errorf("PlayStarted = %v, want reset to a recent time after %v", s.PlayStarted, oldStart)
	}
	if time.Since(s.PlayStarted) > time.Minute {
		t.Errorf("PlayStarted not reset to ~now: %v", s.PlayStarted)
	}
}

func TestMarkAbsentStopped(t *testing.T) {
	withMedia := plexapi.SessionMetadata{Media: []plexapi.MediaInfo{{Bitrate: 1000}}}
	noMedia := plexapi.SessionMetadata{Title: "no media"}

	tests := []struct {
		name           string
		state          State
		meta           plexapi.SessionMetadata
		present        []string
		wantState      State
		wantTimeBanked bool
	}{
		{
			name:           "absent playing with media banks play time",
			state:          StatePlaying,
			meta:           withMedia,
			present:        nil,
			wantState:      StateStopped,
			wantTimeBanked: true,
		},
		{
			name:           "absent playing without media banks play time",
			state:          StatePlaying,
			meta:           noMedia,
			present:        nil,
			wantState:      StateStopped,
			wantTimeBanked: true,
		},
		{
			name:           "absent paused transitions to stopped without banking",
			state:          StatePaused,
			meta:           withMedia,
			present:        nil,
			wantState:      StateStopped,
			wantTimeBanked: false,
		},
		{
			name:           "present playing session is left untouched",
			state:          StatePlaying,
			meta:           withMedia,
			present:        []string{"s1"},
			wantState:      StatePlaying,
			wantTimeBanked: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewTracker()
			tracker.mu.Lock()
			tracker.Sessions["s1"] = Session{
				State:       tc.state,
				PlayStarted: time.Now().Add(-10 * time.Second),
				LastUpdate:  time.Now().Add(-10 * time.Second),
				Meta:        tc.meta,
			}
			tracker.mu.Unlock()

			tracker.MarkAbsentStopped(tc.present)

			tracker.mu.Lock()
			defer tracker.mu.Unlock()
			s := tracker.Sessions["s1"]
			if s.State != tc.wantState {
				t.Errorf("state = %q, want %q", s.State, tc.wantState)
			}
			if (s.PrevPlayedTime > 0) != tc.wantTimeBanked {
				t.Errorf("PrevPlayedTime = %v, wantBanked = %v", s.PrevPlayedTime, tc.wantTimeBanked)
			}
		})
	}
}

func TestMarkAbsentStopped_already_stopped_not_rebanked(t *testing.T) {
	tracker := NewTracker()
	stoppedAt := time.Now().Add(-2 * time.Minute)
	tracker.mu.Lock()
	tracker.Sessions["gone"] = Session{
		State:          StateStopped,
		PlayStarted:    time.Now().Add(-10 * time.Minute),
		LastUpdate:     stoppedAt,
		PrevPlayedTime: 5 * time.Second,
		Meta:           plexapi.SessionMetadata{Media: []plexapi.MediaInfo{{Bitrate: 1000}}},
	}
	tracker.mu.Unlock()

	tracker.MarkAbsentStopped(nil)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	s := tracker.Sessions["gone"]
	if s.PrevPlayedTime != 5*time.Second {
		t.Errorf("PrevPlayedTime = %v, want 5s unchanged (already-stopped session must not re-bank)", s.PrevPlayedTime)
	}
	if !s.LastUpdate.Equal(stoppedAt) {
		t.Errorf("LastUpdate = %v, want unchanged %v (early continue must skip the LastUpdate bump so Prune can still reclaim)", s.LastUpdate, stoppedAt)
	}
}

func TestMarkAbsentStopped_normalizes_present_keys(t *testing.T) {
	tracker := NewTracker()
	longKey := strings.Repeat("k", MaxSessionKeyLen+30)
	// Update stores the session under the normalized (truncated) key.
	tracker.Update(longKey, StatePlaying, &plexapi.SessionMetadata{Title: "long"}, nil)

	// Reporting the SAME long key as present must normalize to the stored key,
	// so the session counts as present and is not transitioned to stopped.
	tracker.MarkAbsentStopped([]string{longKey})

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	s := tracker.Sessions[normalizeKey(longKey)]
	if s.State != StatePlaying {
		t.Errorf("state = %q, want playing (a present key must be normalized to match the stored key)", s.State)
	}
}

func TestNormalizeKey_continuation_bytes_walk_to_zero(t *testing.T) {
	// A session key longer than MaxSessionKeyLen made entirely of UTF-8
	// continuation bytes (0x80) has no rune-start byte, so the boundary walk
	// decrements i from MaxSessionKeyLen down to 0. The guard must stop at zero
	// (i > 0): a >= guard would index id[0], step to -1, and panic on id[:-1].
	input := strings.Repeat("\x80", MaxSessionKeyLen+1)

	got := normalizeKey(input)

	if got != "" {
		t.Errorf("normalizeKey(all-continuation-bytes len=%d) = %q (len=%d), want %q",
			len(input), got, len(got), "")
	}
	if len(got) > MaxSessionKeyLen {
		t.Errorf("normalizeKey result len %d exceeds MaxSessionKeyLen %d", len(got), MaxSessionKeyLen)
	}
}

func TestSnapshotSessions_returns_independent_copy(t *testing.T) {
	tracker := NewTracker()
	tracker.Update("s1", StatePlaying, nil, nil)

	snap := tracker.SnapshotSessions()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}

	// Mutating the returned map must not affect the tracker's map: the collector iterates the
	// snapshot outside the tracker lock, so a shared (non-copied) map would be a data race.
	snap["s2"] = Session{State: StatePlaying}
	delete(snap, "s1")

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if _, ok := tracker.Sessions["s1"]; !ok {
		t.Error("deleting from the snapshot removed s1 from the tracker (map not copied)")
	}
	if _, ok := tracker.Sessions["s2"]; ok {
		t.Error("adding to the snapshot leaked s2 into the tracker (map not copied)")
	}
}

func TestTrackerUpdate_stop_without_media_banks_time(t *testing.T) {
	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["s1"] = Session{
		State:       StatePlaying,
		PlayStarted: time.Now().Add(-10 * time.Second),
		LastUpdate:  time.Now(),
	}
	tracker.mu.Unlock()

	tracker.Update("s1", StateStopped, nil, nil)

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	s := tracker.Sessions["s1"]
	if s.PrevPlayedTime == 0 {
		t.Error("PrevPlayedTime should be banked on a playing->stopped transition even without media")
	}
}
