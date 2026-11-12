package sessions

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
)

const prunedSummaryMsg = "pruned expired sessions"

func TestTrackerPrune(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	tracker.Sessions["old"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-2 * sessionTimeout),
	}
	tracker.Sessions["recent"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now(),
	}
	tracker.Sessions["playing_fresh"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * sessionTimeout),
	}
	tracker.Sessions["playing_stale"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * staleSessionTimeout),
	}
	tracker.Sessions["paused_stale"] = Session{
		State:      State("paused"),
		LastUpdate: time.Now().Add(-2 * staleSessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["old"]; ok {
		t.Error("old stopped session should be pruned")
	}
	if _, ok := tracker.Sessions["recent"]; !ok {
		t.Error("recent stopped session should be kept")
	}
	if _, ok := tracker.Sessions["playing_fresh"]; !ok {
		t.Error("playing session idle less than staleSessionTimeout should be kept")
	}
	if _, ok := tracker.Sessions["playing_stale"]; ok {
		t.Error("playing session idle longer than staleSessionTimeout should be pruned")
	}
	if _, ok := tracker.Sessions["paused_stale"]; ok {
		t.Error("paused session idle longer than staleSessionTimeout should be pruned")
	}
}

// TestTrackerPrune_stale_boundary covers the threshold edge: a non-stopped
// session idle for less than staleSessionTimeout must NOT be pruned, one
// idle past it must be.
func TestTrackerPrune_stale_boundary(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	// Under the threshold — kept.
	tracker.Sessions["under_threshold"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-staleSessionTimeout + time.Minute),
	}
	// Past the threshold — pruned.
	tracker.Sessions["past_threshold"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-staleSessionTimeout - time.Second),
	}
	tracker.mu.Unlock()

	tracker.prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["under_threshold"]; !ok {
		t.Error("session idle under staleSessionTimeout should be kept")
	}
	if _, ok := tracker.Sessions["past_threshold"]; ok {
		t.Error("session idle past staleSessionTimeout should be pruned")
	}
}

// TestSessionTrackerPrune_exact_timeout_boundary checks the stopped-session
// timeout edge: a session stopped within sessionTimeout must be kept and one
// stopped past it must be pruned (the guard is strictly greater-than, so the
// boundary itself is retained).
func TestSessionTrackerPrune_exact_timeout_boundary(t *testing.T) {
	tracker := NewTracker()

	tracker.mu.Lock()
	tracker.Sessions["barely_within"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-sessionTimeout + 100*time.Millisecond),
	}
	tracker.Sessions["well_past"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-sessionTimeout - time.Second),
	}
	tracker.mu.Unlock()

	tracker.prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.Sessions["barely_within"]; !ok {
		t.Error("barely_within should be kept (within timeout)")
	}
	if _, ok := tracker.Sessions["well_past"]; ok {
		t.Error("well_past should be pruned (past timeout)")
	}
}

func TestTrackerPrune_no_removals_emits_no_summary(t *testing.T) {
	logs := capture.Default(t)

	tracker := NewTracker()
	tracker.mu.Lock()
	// A fresh stopped session well within the timeout — not removable.
	tracker.Sessions["keep"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now(),
	}
	tracker.mu.Unlock()

	tracker.prune()

	if logs.Contains(prunedSummaryMsg) {
		t.Errorf("prune() with no removals emitted %q summary, want none", prunedSummaryMsg)
	}
}

// TestTrackerPrune_stopped_removal_logs_stopped_count verifies that pruning a
// single expired stopped session emits the summary with stopped=1, stale=0.
func TestTrackerPrune_stopped_removal_logs_stopped_count(t *testing.T) {
	logs := capture.Default(t)

	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["expired"] = Session{
		State:      StateStopped,
		LastUpdate: time.Now().Add(-2 * sessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.prune()

	if !logs.Contains(prunedSummaryMsg) {
		t.Fatalf("prune() removing 1 stopped session emitted no %q summary, want one", prunedSummaryMsg)
	}
	if got, ok := logs.AttrValue(prunedSummaryMsg, "stopped"); !ok || got != "1" {
		t.Errorf("summary stopped count = %q (found=%v), want 1", got, ok)
	}
	if got, ok := logs.AttrValue(prunedSummaryMsg, "stale"); !ok || got != "0" {
		t.Errorf("summary stale count = %q (found=%v), want 0", got, ok)
	}
}

// TestTrackerPrune_stale_removal_logs_stale_count verifies that pruning a
// single orphaned non-stopped session emits the summary with stale=1,
// stopped=0.
func TestTrackerPrune_stale_removal_logs_stale_count(t *testing.T) {
	logs := capture.Default(t)

	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["orphan"] = Session{
		State:      StatePlaying,
		LastUpdate: time.Now().Add(-2 * staleSessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.prune()

	if !logs.Contains(prunedSummaryMsg) {
		t.Fatalf("prune() removing 1 stale session emitted no %q summary, want one", prunedSummaryMsg)
	}
	if got, ok := logs.AttrValue(prunedSummaryMsg, "stale"); !ok || got != "1" {
		t.Errorf("summary stale count = %q (found=%v), want 1", got, ok)
	}
	if got, ok := logs.AttrValue(prunedSummaryMsg, "stopped"); !ok || got != "0" {
		t.Errorf("summary stopped count = %q (found=%v), want 0", got, ok)
	}
}

// TestPruneTimeouts_match_documented_contract pins the absolute timeout
// constants to the values the README and tracker.go comments document, so a
// silent edit to either default fails a test (the offset-based boundary tests
// would still pass after such a drift).
func TestPruneTimeouts_match_documented_contract(t *testing.T) {
	if sessionTimeout != time.Minute {
		t.Errorf("sessionTimeout = %v, want 1m0s (README: sessions pruned after 60s of inactivity)", sessionTimeout)
	}
	if staleSessionTimeout != 5*time.Minute {
		t.Errorf("staleSessionTimeout = %v, want 5m0s (documented 5-minute stale-session timeout)", staleSessionTimeout)
	}
}

// A full map is one standing state: one WARN when it starts refusing
// sessions, one INFO with the episode's drop count when a slot frees, while
// the counter still counts every refused session.
func TestTracker_logs_a_full_map_once(t *testing.T) {
	logs := capture.Default(t)
	tracker := NewTracker()
	tracker.mu.Lock()
	tracker.Sessions["ended"] = Session{State: StateStopped, LastUpdate: time.Now().Add(-2 * sessionTimeout)}
	for i := range maxTrackedSessions - 1 {
		tracker.Sessions[fmt.Sprintf("s%d", i)] = Session{State: StatePlaying, LastUpdate: time.Now()}
	}
	tracker.mu.Unlock()

	for i := range 5 {
		tracker.Update(fmt.Sprintf("new%d", i), StatePlaying, nil, nil)
	}
	if got := logs.CountLevel(slog.LevelWarn, "session map full"); got != 1 {
		t.Errorf("WARN session map full logged %d times for 5 drops, want 1", got)
	}
	if _, dropped := tracker.Counts(); dropped != 5 {
		t.Errorf("dropped = %d, want 5", dropped)
	}
	if !logs.HasAttr("session map full", "tracked", "256") || !logs.HasAttr("session map full", "cap", "256") {
		t.Errorf("session map full lacks tracked=256 and cap=256: %v", logs.Records())
	}

	tracker.prune()
	if got := logs.CountLevel(slog.LevelInfo, "session map has room again"); got != 1 {
		t.Errorf("INFO session map has room again logged %d times, want 1", got)
	}
	if !logs.HasAttr("session map has room again", "dropped", "5") {
		t.Errorf("session map has room again lacks dropped=5: %v", logs.Records())
	}

	tracker.Update("fits", StatePlaying, nil, nil)
	tracker.Update("refused", StatePlaying, nil, nil)
	if got := logs.CountLevel(slog.LevelWarn, "session map full"); got != 2 {
		t.Errorf("WARN session map full logged %d times after the map filled again, want 2", got)
	}
}
