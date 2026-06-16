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

// TestTrackerPrune_no_removals_emits_no_summary verifies that when nothing is
// reclaimed, Prune does NOT emit the "pruned expired sessions" debug summary.
// This pins the `pruned > 0 || stale > 0` boundary: a `>=` mutant would emit
// the summary even though both counters are zero.
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
// This pins `pruned++` (a `--` mutant makes pruned negative, closing the
// `pruned > 0` gate so no summary is emitted) and the `pruned > 0` negation.
func TestTrackerPrune_stopped_removal_logs_stopped_count(t *testing.T) {
	getRecords := captureSlog(t, slog.LevelDebug)

	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["expired"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-2 * SessionTimeout),
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
// stopped=0. This pins `stale++` (a `--` mutant closes the `stale > 0` gate)
// and the `stale > 0` negation in the summary guard.
func TestTrackerPrune_stale_removal_logs_stale_count(t *testing.T) {
	getRecords := captureSlog(t, slog.LevelDebug)

	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["orphan"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * StaleSessionTimeout),
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

// TestTrackerPrune_removes_transcode_index_entry verifies that pruning a
// session that carries a TranscodeKey also evicts its transcodeIndex entry.
// This pins the `s.TranscodeKey != ""` guard: a `== ""` negation mutant would
// skip the delete and leave a stale index entry behind.
func TestTrackerPrune_removes_transcode_index_entry(t *testing.T) {
	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["s1"] = Session{
		State:        StateStopped,
		LastUpdate:   time.Now().Add(-2 * SessionTimeout),
		TranscodeKey: "tc1",
	}
	tracker.transcodeIndex["tc1"] = "s1"
	tracker.mu.Unlock()

	tracker.Prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.transcodeIndex["tc1"]; ok {
		t.Error("transcodeIndex entry for pruned session's TranscodeKey should be deleted")
	}
	if _, ok := tracker.Sessions["s1"]; ok {
		t.Error("stopped expired session should be pruned")
	}
}
