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
	if tracker.TotalEstimatedKBits == 0 {
		t.Error("TotalEstimatedKBits should be > 0")
	}
}

func TestTrackerPrune(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	tracker.Sessions["old"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-2 * SessionTimeout),
	}
	tracker.Sessions["recent"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now(),
	}
	tracker.Sessions["playing_fresh"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * SessionTimeout),
	}
	tracker.Sessions["playing_stale"] = Session{
		// Non-stopped but silent longer than StaleSessionTimeout — orphaned.
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * StaleSessionTimeout),
	}
	tracker.Sessions["paused_stale"] = Session{
		State:      State("paused"),
		LastUpdate: time.Now().Add(-2 * StaleSessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["old"]; ok {
		t.Error("old stopped session should be pruned")
	}
	if _, ok := tracker.Sessions["recent"]; !ok {
		t.Error("recent stopped session should be kept")
	}
	if _, ok := tracker.Sessions["playing_fresh"]; !ok {
		t.Error("playing session idle less than StaleSessionTimeout should be kept")
	}
	if _, ok := tracker.Sessions["playing_stale"]; ok {
		t.Error("playing session idle longer than StaleSessionTimeout should be pruned (h-f3)")
	}
	if _, ok := tracker.Sessions["paused_stale"]; ok {
		t.Error("paused session idle longer than StaleSessionTimeout should be pruned (h-f3)")
	}
}

// TestTrackerPrune_stale_boundary covers the threshold edge: a non-stopped
// session idle for less than StaleSessionTimeout must NOT be pruned, one
// idle past it must be.
func TestTrackerPrune_stale_boundary(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	// Well under the threshold — should be kept.
	tracker.Sessions["under_threshold"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-StaleSessionTimeout + time.Minute),
	}
	// Just past the threshold — should be pruned.
	tracker.Sessions["past_threshold"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-StaleSessionTimeout - time.Second),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["under_threshold"]; !ok {
		t.Error("session idle under StaleSessionTimeout should be kept")
	}
	if _, ok := tracker.Sessions["past_threshold"]; ok {
		t.Error("session idle past StaleSessionTimeout should be pruned")
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
	// Targets lived mutants at lines 523 (len(s.Meta.Media) > 0 boundary)
	// and 526 (elapsed.Seconds() * float64(bitrate) arithmetic).
	// Verifies that stopping a playing session with media accumulates
	// totalEstimatedKBits correctly.
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
	// This is the inverse test for the len(s.Meta.Media) > 0 check.
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

func TestNormalizeKey(t *testing.T) {
	// Direct unit test for the normalizeKey helper.
	short := "abc123"
	if got := normalizeKey(short); got != short {
		t.Errorf("normalizeKey(%q) = %q, want unchanged", short, got)
	}

	exact := strings.Repeat("x", MaxSessionKeyLen)
	if got := normalizeKey(exact); got != exact {
		t.Errorf("normalizeKey(len=%d) should be unchanged", MaxSessionKeyLen)
	}

	long := strings.Repeat("y", MaxSessionKeyLen+50)
	want := long[:MaxSessionKeyLen]
	if got := normalizeKey(long); got != want {
		t.Errorf("normalizeKey(len=%d) = len %d, want len %d", len(long), len(got), MaxSessionKeyLen)
	}
}

func TestSessionTrackerPrune_exact_timeout_boundary(t *testing.T) {
	// Targets lived mutant at line 542 (time.Since > SessionTimeout boundary).
	// A session stopped exactly at the timeout boundary should NOT be pruned
	// (> not >=).
	tracker := NewTracker()

	// Session stopped just barely within the timeout — should be kept
	tracker.mu.Lock()
	tracker.Sessions["barely_within"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-SessionTimeout + 100*time.Millisecond),
	}
	// Session stopped well past the timeout — should be pruned
	tracker.Sessions["well_past"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-SessionTimeout - time.Second),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["barely_within"]; !ok {
		t.Error("barely_within should be kept (within timeout)")
	}
	if _, ok := tracker.Sessions["well_past"]; ok {
		t.Error("well_past should be pruned (past timeout)")
	}
}
