package sessions

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// recordingHandler is a minimal slog.Handler that captures emitted records
// in memory so tests can assert on observable log side-effects (the only
// output of Prune's pruned/stale aggregate counters).
type recordingHandler struct {
	records []slog.Record
	level   slog.Level
	mu      sync.Mutex
}

func (h *recordingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

// captureSlog redirects slog's default logger to an in-memory handler at the
// given level for the duration of the test, restoring the previous default on
// cleanup. The returned function yields a snapshot of captured records.
func captureSlog(t *testing.T, level slog.Level) func() []slog.Record {
	t.Helper()
	prev := slog.Default()
	h := &recordingHandler{level: level}
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []slog.Record {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := make([]slog.Record, len(h.records))
		copy(out, h.records)
		return out
	}
}

// findRecord returns the first captured record whose message matches msg.
func findRecord(records []slog.Record, msg string) (slog.Record, bool) {
	for _, r := range records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// recordInt64 extracts the int64 value of attribute key from r.
func recordInt64(r slog.Record, key string) (int64, bool) {
	var v int64
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.Int64()
			found = true
			return false
		}
		return true
	})
	return v, found
}

const prunedSummaryMsg = "pruned expired sessions"

func TestTrackerPrune(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	tracker.Sessions["old"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-2 * defaultSessionTimeout),
	}
	tracker.Sessions["recent"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now(),
	}
	tracker.Sessions["playing_fresh"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * defaultSessionTimeout),
	}
	tracker.Sessions["playing_stale"] = Session{
		// Non-stopped but silent longer than defaultStaleSessionTimeout — orphaned.
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * defaultStaleSessionTimeout),
	}
	tracker.Sessions["paused_stale"] = Session{
		State:      State("paused"),
		LastUpdate: time.Now().Add(-2 * defaultStaleSessionTimeout),
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
		t.Error("playing session idle less than defaultStaleSessionTimeout should be kept")
	}
	if _, ok := tracker.Sessions["playing_stale"]; ok {
		t.Error("playing session idle longer than defaultStaleSessionTimeout should be pruned")
	}
	if _, ok := tracker.Sessions["paused_stale"]; ok {
		t.Error("paused session idle longer than defaultStaleSessionTimeout should be pruned")
	}
}

// TestTrackerPrune_stale_boundary covers the threshold edge: a non-stopped
// session idle for less than defaultStaleSessionTimeout must NOT be pruned, one
// idle past it must be.
func TestTrackerPrune_stale_boundary(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	// Well under the threshold — should be kept.
	tracker.Sessions["under_threshold"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-defaultStaleSessionTimeout + time.Minute),
	}
	// Just past the threshold — should be pruned.
	tracker.Sessions["past_threshold"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-defaultStaleSessionTimeout - time.Second),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["under_threshold"]; !ok {
		t.Error("session idle under defaultStaleSessionTimeout should be kept")
	}
	if _, ok := tracker.Sessions["past_threshold"]; ok {
		t.Error("session idle past defaultStaleSessionTimeout should be pruned")
	}
}

// TestSessionTrackerPrune_exact_timeout_boundary checks the stopped-session
// timeout edge: a session stopped within defaultSessionTimeout must be kept and one
// stopped past it must be pruned (the guard is strictly greater-than, so the
// boundary itself is retained).
func TestSessionTrackerPrune_exact_timeout_boundary(t *testing.T) {
	tracker := NewTracker()

	// Session stopped just barely within the timeout — should be kept
	tracker.mu.Lock()
	tracker.Sessions["barely_within"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-defaultSessionTimeout + 100*time.Millisecond),
	}
	// Session stopped well past the timeout — should be pruned
	tracker.Sessions["well_past"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-defaultSessionTimeout - time.Second),
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

// TestTrackerPrune_no_removals_emits_no_summary verifies that when nothing is
// reclaimed, Prune does NOT emit the "pruned expired sessions" debug summary.
func TestTrackerPrune_no_removals_emits_no_summary(t *testing.T) {
	getRecords := captureSlog(t, slog.LevelDebug)

	tracker := NewTracker()
	tracker.mu.Lock()
	// A fresh stopped session well within the timeout — not removable.
	tracker.Sessions["keep"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now(),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	if _, ok := findRecord(getRecords(), prunedSummaryMsg); ok {
		t.Errorf("Prune() with no removals emitted %q summary, want none", prunedSummaryMsg)
	}
}

// TestTrackerPrune_stopped_removal_logs_stopped_count verifies that pruning a
// single expired stopped session emits the summary with stopped=1, stale=0.
func TestTrackerPrune_stopped_removal_logs_stopped_count(t *testing.T) {
	getRecords := captureSlog(t, slog.LevelDebug)

	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["expired"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-2 * defaultSessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	rec, ok := findRecord(getRecords(), prunedSummaryMsg)
	if !ok {
		t.Fatalf("Prune() removing 1 stopped session emitted no %q summary, want one", prunedSummaryMsg)
	}
	if stopped, _ := recordInt64(rec, "stopped"); stopped != 1 {
		t.Errorf("summary stopped count = %d, want 1", stopped)
	}
	if stale, _ := recordInt64(rec, "stale"); stale != 0 {
		t.Errorf("summary stale count = %d, want 0", stale)
	}
}

// TestTrackerPrune_stale_removal_logs_stale_count verifies that pruning a
// single orphaned non-stopped session emits the summary with stale=1,
// stopped=0.
func TestTrackerPrune_stale_removal_logs_stale_count(t *testing.T) {
	getRecords := captureSlog(t, slog.LevelDebug)

	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["orphan"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * defaultStaleSessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.Prune()

	rec, ok := findRecord(getRecords(), prunedSummaryMsg)
	if !ok {
		t.Fatalf("Prune() removing 1 stale session emitted no %q summary, want one", prunedSummaryMsg)
	}
	if stale, _ := recordInt64(rec, "stale"); stale != 1 {
		t.Errorf("summary stale count = %d, want 1", stale)
	}
	if stopped, _ := recordInt64(rec, "stopped"); stopped != 0 {
		t.Errorf("summary stopped count = %d, want 0", stopped)
	}
}

// TestPruneTimeouts_match_documented_contract pins the absolute timeout
// constants to the values the README and tracker.go comments document, so a
// silent edit to either default fails a test (the offset-based boundary tests
// would still pass after such a drift).
func TestPruneTimeouts_match_documented_contract(t *testing.T) {
	if defaultSessionTimeout != time.Minute {
		t.Errorf("defaultSessionTimeout = %v, want 1m0s (README: sessions pruned after 60s of inactivity)", defaultSessionTimeout)
	}
	if defaultStaleSessionTimeout != 5*time.Minute {
		t.Errorf("defaultStaleSessionTimeout = %v, want 5m0s (documented 5-minute stale-session timeout)", defaultStaleSessionTimeout)
	}
}
