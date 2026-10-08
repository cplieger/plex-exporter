package history

import (
	"context"
	"errors"
	"iter"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/plexapi/v2"
)

// fakeSource serves rows viewed at or after since, from the page's Start
// offset, in pages of the asked size, calling wait before each page.
// failPage >= 0 makes that page an error; calls counts walks.
type fakeSource struct {
	rows     []plexapi.HistoryEntry
	since    []int64
	starts   []int
	failPage int
	calls    int
	mu       sync.Mutex
}

func (f *fakeSource) set(fn func(*fakeSource)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSource) walks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSource) WalkHistory(ctx context.Context, since int64, page plexapi.Page, wait func(context.Context) error) iter.Seq2[plexapi.HistoryEntry, error] {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.since = append(f.since, since)
	f.starts = append(f.starts, page.Start)
	rows, failPage := slices.Clone(f.rows), f.failPage
	return func(yield func(plexapi.HistoryEntry, error) bool) {
		var sel []plexapi.HistoryEntry
		for _, r := range rows {
			if r.ViewedAt >= since || r.ViewedAt == 0 {
				sel = append(sel, r)
			}
		}
		sel = sel[min(page.Start, len(sel)):]
		for p := 0; p*page.Size < len(sel); p++ {
			if err := wait(ctx); err != nil {
				yield(plexapi.HistoryEntry{}, err)
				return
			}
			if p == failPage {
				yield(plexapi.HistoryEntry{}, errors.New("plex restarted"))
				return
			}
			for _, r := range sel[p*page.Size : min(len(sel), (p+1)*page.Size)] {
				if !yield(r, nil) {
					return
				}
			}
		}
	}
}

type countingPacer struct{ waits int }

func (p *countingPacer) Wait(context.Context) error { p.waits++; return nil }

func newStore(src Source) *Store {
	return New(src, &countingPacer{})
}

// mustBootstrap runs the full read a test builds on and stops the test when
// it does not complete, so a broken setup is not reported as a later failure.
func mustBootstrap(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	if st := s.Bootstrap(ctx); st != StatusComplete {
		t.Fatalf("Setup: Bootstrap() = %q, want complete", st)
	}
}

func row(key string, viewedAt int64) plexapi.HistoryEntry {
	return plexapi.HistoryEntry{RatingKey: key, ViewedAt: viewedAt}
}

func TestBootstrap_keeps_the_newest_play_per_item(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("7", 100), row("7", 300), row("8", 200), row("7", 300)}}
	s := newStore(src)
	if st := s.Bootstrap(t.Context()); st != StatusComplete {
		t.Fatalf("Bootstrap() = %q, want complete", st)
	}
	if got := s.Visit(7, 0); got != 300 {
		t.Errorf("Visit(7, 0) = %d, want 300", got)
	}
	if v := s.View(); !v.Current || v.Unmatched != 0 {
		t.Errorf("View() = %+v, want current with no unmatched rows", v)
	}
}

func TestBootstrap_counts_unusable_rows_and_never_writes_them(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{
		row("5", 100),
		{RatingKey: "6", HistoryKey: "/status/sessions/history/1"},
		{RatingKey: "", ViewedAt: 120, HistoryKey: "/status/sessions/history/2"},
		{RatingKey: "+12", ViewedAt: 130, HistoryKey: "/status/sessions/history/3"},
	}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	if v := s.View(); v.Unmatched != 2 || v.Deleted != 1 {
		t.Errorf("View() Unmatched, Deleted = %d, %d, want 2, 1", v.Unmatched, v.Deleted)
	}
	for _, k := range []uint64{6, 12, 0} {
		if got := s.Visit(k, 0); got != 0 {
			t.Errorf("Visit(%d, 0) = %d, want 0: an unusable row is never written", k, got)
		}
	}
	src.rows = append(src.rows, row("9", 500))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() error = %v", err)
	}
	if v := s.View(); v.Unmatched != 2 || v.Deleted != 1 {
		t.Errorf("after a clean increment View() Unmatched, Deleted = %d, %d, want 2, 1: only a new full read clears them", v.Unmatched, v.Deleted)
	}
	src.rows = src.rows[:1]
	mustBootstrap(t.Context(), t, s)
	if v := s.View(); v.Unmatched != 0 || v.Deleted != 0 {
		t.Errorf("after a clean bootstrap View() Unmatched, Deleted = %d, %d, want 0, 0", v.Unmatched, v.Deleted)
	}
}

// Plays of deleted items arrive with no item key. They are counted once
// each, an overlapping re-read included, and never count as unmatched.
func TestBootstrap_counts_deleted_item_rows_apart_from_unmatched(t *testing.T) {
	deleted := func(id string, at int64) plexapi.HistoryEntry {
		return plexapi.HistoryEntry{HistoryKey: "/status/sessions/history/" + id, ViewedAt: at, AccountID: 2}
	}
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{
		row("5", 1000), deleted("1", 1000), deleted("2", 1000), deleted("3", 1100),
	}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	src.rows = append(src.rows, deleted("4", 1200), row("6", 1200))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() error = %v", err)
	}
	if v := s.View(); v.Deleted != 4 || v.Unmatched != 0 {
		t.Errorf("View() Deleted, Unmatched = %d, %d, want 4, 0; reads started at %v", v.Deleted, v.Unmatched, src.since)
	}
	if got := s.Visit(6, 0); got != 1200 {
		t.Errorf("Visit(6, 0) = %d, want 1200", got)
	}
}

// A full read can hold MaxCountedRows rows, so its count is exact; later
// reads saturate at the cap instead of growing without bound.
func TestBootstrap_counts_every_unmatched_row_of_a_full_read(t *testing.T) {
	rows := make([]plexapi.HistoryEntry, 0, MaxPages*PageSize)
	rows = append(rows, row("1", 1000))
	for i := range MaxPages*PageSize - 1 {
		rows = append(rows, plexapi.HistoryEntry{RatingKey: "bad", HistoryKey: "/status/sessions/history/" + strconv.Itoa(i), ViewedAt: 1})
	}
	src := &fakeSource{failPage: -1, rows: rows}
	s := newStore(src)
	if st := s.Bootstrap(t.Context()); st != StatusComplete {
		t.Fatalf("Bootstrap() = %q, want complete", st)
	}
	if v := s.View(); v.Unmatched != MaxPages*PageSize-1 {
		t.Errorf("after a full read View().Unmatched = %d, want %d", v.Unmatched, MaxPages*PageSize-1)
	}
	src.rows = append(src.rows,
		plexapi.HistoryEntry{RatingKey: "bad", HistoryKey: "/status/sessions/history/new-1", ViewedAt: 2000},
		plexapi.HistoryEntry{RatingKey: "bad", HistoryKey: "/status/sessions/history/new-2", ViewedAt: 2000})
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() error = %v", err)
	}
	if v := s.View(); v.Unmatched != MaxCountedRows {
		t.Errorf("after two more unmatched rows View().Unmatched = %d, want the cap %d", v.Unmatched, MaxCountedRows)
	}
}

// bigKeySource yields n distinct unmatched rows, each with a fresh 1 MiB
// item key and no history key, and keeps none of them.
type bigKeySource struct{ n int }

func (b bigKeySource) WalkHistory(context.Context, int64, plexapi.Page, func(context.Context) error) iter.Seq2[plexapi.HistoryEntry, error] {
	return func(yield func(plexapi.HistoryEntry, error) bool) {
		for i := range b.n {
			if !yield(plexapi.HistoryEntry{RatingKey: strings.Repeat("x", 1<<20) + strconv.Itoa(i), ViewedAt: 1}, nil) {
				return
			}
		}
	}
}

func TestBootstrap_unmatched_rows_do_not_retain_their_keys(t *testing.T) {
	const n = 64
	s := newStore(bigKeySource{n: n})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	mustBootstrap(t.Context(), t, s)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if v := s.View(); v.Unmatched != n {
		t.Errorf("View().Unmatched = %d, want %d", v.Unmatched, n)
	}
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 8<<20 {
		t.Errorf("live heap grew %d MiB after reading %d unmatched rows with 1 MiB keys, want under 8 MiB", grew>>20, n)
	}
	runtime.KeepAlive(s)
}

func TestBootstrap_failure_discards_the_map(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 10)}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	before := s.View().Gen
	src.failPage = 0
	if st := s.Bootstrap(t.Context()); st != StatusFailed {
		t.Fatalf("Bootstrap() = %q, want failed", st)
	}
	v := s.View()
	if v.Current || s.Visit(1, 0) != 0 {
		t.Errorf("after a failed bootstrap Current=%v Visit(1, 0)=%d, want false and 0", v.Current, s.Visit(1, 0))
	}
	if v.Gen <= before {
		t.Errorf("Gen = %d after a failed bootstrap, want past %d", v.Gen, before)
	}
}

func TestBootstrap_over_the_page_limit(t *testing.T) {
	rows := make([]plexapi.HistoryEntry, MaxPages*PageSize+1)
	for i := range rows {
		rows[i] = row("1", int64(i+1))
	}
	s := newStore(&fakeSource{failPage: -1, rows: rows})
	if st := s.Bootstrap(t.Context()); st != StatusOverLimit {
		t.Fatalf("Bootstrap() = %q, want over_limit", st)
	}
	if s.Visit(1, 0) != 0 {
		t.Errorf("Visit(1, 0) = %d, want 0: an over-limit read is discarded", s.Visit(1, 0))
	}
}

func TestIncremental_failure_keeps_state_and_cursor(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 1000)}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	src.failPage = 0
	if err := s.Incremental(t.Context()); err == nil {
		t.Fatal("Incremental() error = nil, want the page error")
	}
	if s.Visit(1, 0) != 1000 || s.View().Status != StatusComplete {
		t.Errorf("after a failed increment Visit(1, 0)=%d Status=%q, want 1000 and complete", s.Visit(1, 0), s.View().Status)
	}
	src.failPage = -1
	src.rows = append(src.rows, row("2", 1200))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() error = %v", err)
	}
	if got := src.since[len(src.since)-1]; got != 1000-incrementalOverlap {
		t.Errorf("increment read from %d, want %d (cursor minus the overlap)", got, 1000-incrementalOverlap)
	}
	if s.Visit(2, 0) != 1200 {
		t.Errorf("Visit(2, 0) = %d, want 1200", s.Visit(2, 0))
	}
}

// A capped increment moves the cursor past unmatched rows too, so a long
// run of them cannot hide the plays after it.
func TestIncremental_cap_moves_past_unmatched_rows(t *testing.T) {
	rows := []plexapi.HistoryEntry{row("1", 1000)}
	src := &fakeSource{failPage: -1, rows: rows}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	for i := range MaxIncrementalPages * PageSize {
		src.rows = append(src.rows, row("bad", int64(2000+i)))
	}
	src.rows = append(src.rows, row("9", 2000+MaxIncrementalPages*PageSize))
	for i := range 2 {
		if err := s.Incremental(t.Context()); err != nil {
			t.Fatalf("Incremental() #%d error = %v", i+1, err)
		}
	}
	if got := s.Visit(9, 0); got != 2000+MaxIncrementalPages*PageSize {
		t.Errorf("Visit(9, 0) after two capped increments = %d, want %d; reads started at %v",
			got, 2000+MaxIncrementalPages*PageSize, src.since)
	}
}

// A cap-sized run of rows sharing one second cannot be stepped over by
// timestamp, so successive increments page on from the row offset and reach
// both a play in that same second and one after it.
func TestIncremental_capped_read_inside_one_second_reaches_the_rows_after_it(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 1000)}}
	s := newStore(src)
	if st := s.Bootstrap(t.Context()); st != StatusComplete {
		t.Fatalf("Bootstrap() = %q, want complete", st)
	}
	for range MaxIncrementalPages * PageSize {
		src.rows = append(src.rows, row("bad", 2000))
	}
	src.rows = append(src.rows, row("8", 2000), row("9", 2001))
	for i := range 2 {
		if err := s.Incremental(t.Context()); err != nil {
			t.Fatalf("Incremental() #%d error = %v", i+1, err)
		}
	}
	if got8, got9 := s.Visit(8, 0), s.Visit(9, 0); got8 != 2000 || got9 != 2001 {
		t.Errorf("Visit(8, 0), Visit(9, 0) after two capped increments = %d, %d, want 2000, 2001; reads started at since %v, offset %v",
			got8, got9, src.since, src.starts)
	}
}

// A window longer than two caps is drained across successive reads, each
// starting where the last one stopped.
func TestIncremental_drain_spans_several_capped_reads(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 1000)}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	for range 2 * MaxIncrementalPages * PageSize {
		src.rows = append(src.rows, row("bad", 2000))
	}
	src.rows = append(src.rows, row("9", 2000))
	for i := range 3 {
		if err := s.Incremental(t.Context()); err != nil {
			t.Fatalf("Incremental() #%d error = %v", i+1, err)
		}
	}
	if got := s.Visit(9, 0); got != 2000 {
		t.Errorf("Visit(9, 0) after three increments = %d, want 2000; reads started at offset %v", got, src.starts)
	}
}

// Once the capped window is drained, the next increment returns to the
// cursor minus the overlap, from the first row.
func TestIncremental_resumes_the_overlap_after_a_drained_window(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 1000)}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	for range MaxIncrementalPages * PageSize {
		src.rows = append(src.rows, row("bad", 2000))
	}
	src.rows = append(src.rows, row("9", 2001))
	for i := range 3 {
		if err := s.Incremental(t.Context()); err != nil {
			t.Fatalf("Incremental() #%d error = %v", i+1, err)
		}
	}
	wantSince := []int64{0, 700, 700, 2001 - incrementalOverlap}
	wantStarts := []int{0, 0, MaxIncrementalPages * PageSize, 0}
	if !slices.Equal(src.since, wantSince) || !slices.Equal(src.starts, wantStarts) {
		t.Errorf("reads started at since %v, offset %v, want since %v, offset %v", src.since, src.starts, wantSince, wantStarts)
	}
}

// A failed read while draining keeps the continuation, so the retry pages on
// from the same row.
func TestIncremental_failure_while_draining_keeps_the_offset(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 1000)}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	for range MaxIncrementalPages * PageSize {
		src.rows = append(src.rows, row("bad", 2000))
	}
	src.rows = append(src.rows, row("9", 2000))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() #1 error = %v", err)
	}
	src.failPage = 0
	if err := s.Incremental(t.Context()); err == nil {
		t.Fatal("Incremental() #2 error = nil, want the page error")
	}
	src.failPage = -1
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() #3 error = %v", err)
	}
	if got := s.Visit(9, 0); got != 2000 {
		t.Errorf("Visit(9, 0) = %d, want 2000; reads started at since %v, offset %v", got, src.since, src.starts)
	}
	if got := src.starts[len(src.starts)-1]; got != MaxIncrementalPages*PageSize {
		t.Errorf("retry read from offset %d, want %d", got, MaxIncrementalPages*PageSize)
	}
}

// A full read replaces the read state, so an increment after it starts from
// the rebuilt cursor rather than an older capped window.
func TestBootstrap_closes_an_open_drain(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 1000)}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	for range MaxIncrementalPages * PageSize {
		src.rows = append(src.rows, row("bad", 2000))
	}
	src.rows = append(src.rows, row("9", 3000))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() #1 error = %v", err)
	}
	if st := s.Bootstrap(t.Context()); st != StatusComplete {
		t.Fatalf("Bootstrap() = %q, want complete", st)
	}
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() #2 error = %v", err)
	}
	gotSince, gotStart := src.since[len(src.since)-1], src.starts[len(src.starts)-1]
	if gotSince != 3000-incrementalOverlap || gotStart != 0 {
		t.Errorf("increment after a rebuild read from since %d, offset %d, want %d, 0", gotSince, gotStart, 3000-incrementalOverlap)
	}
}

// The full read's cursor covers its trailing unmatched rows, so the first
// increment starts after them.
func TestBootstrap_cursor_covers_trailing_unmatched_rows(t *testing.T) {
	rows := []plexapi.HistoryEntry{row("1", 1000)}
	for i := range MaxIncrementalPages * PageSize {
		rows = append(rows, row("bad", int64(2000+i)))
	}
	src := &fakeSource{failPage: -1, rows: rows}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	src.rows = append(src.rows, row("9", 2000+MaxIncrementalPages*PageSize))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() error = %v", err)
	}
	if got := s.Visit(9, 0); got != 2000+MaxIncrementalPages*PageSize {
		t.Errorf("Visit(9, 0) after one increment = %d, want %d; reads started at %v",
			got, 2000+MaxIncrementalPages*PageSize, src.since)
	}
}

// Without a history key, the account tells two unmatched plays of one item
// at one second apart.
func TestBootstrap_counts_unmatched_plays_of_two_accounts_apart(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{
		{RatingKey: "+5", ViewedAt: 100, AccountID: 1},
		{RatingKey: "+5", ViewedAt: 100, AccountID: 2},
	}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	if v := s.View(); v.Unmatched != 2 {
		t.Errorf("View().Unmatched = %d, want 2: one row per account", v.Unmatched)
	}
}

func TestView_stale_after_an_hour_without_a_successful_read(t *testing.T) {
	s := newStore(&fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 10)}})
	start := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return start }
	mustBootstrap(t.Context(), t, s)
	s.now = func() time.Time { return start.Add(StaleAfter - time.Second) }
	if !s.View().Current {
		t.Error("View().Current = false just inside StaleAfter, want true")
	}
	s.now = func() time.Time { return start.Add(StaleAfter) }
	if s.View().Current {
		t.Error("View().Current = true at StaleAfter, want false")
	}
}

func TestCatchup_merges_a_late_play(t *testing.T) {
	src := &fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", time.Now().Unix())}}
	s := newStore(src)
	mustBootstrap(t.Context(), t, s)
	late := time.Now().Add(-2 * time.Hour).Unix()
	src.rows = append(src.rows, row("2", late))
	if err := s.Incremental(t.Context()); err != nil {
		t.Fatalf("Incremental() error = %v", err)
	}
	if s.Visit(2, 0) != 0 {
		t.Fatalf("Visit(2, 0) = %d after the increment, want 0: the play is behind the cursor", s.Visit(2, 0))
	}
	for range 2 {
		if err := s.Catchup(t.Context()); err != nil {
			t.Fatalf("Catchup() error = %v", err)
		}
	}
	if s.Visit(2, 0) != late {
		t.Errorf("Visit(2, 0) = %d, want %d", s.Visit(2, 0), late)
	}
}

func TestPrune_removes_only_unvisited_keys_older_than_the_cutoff(t *testing.T) {
	s := newStore(&fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 10), row("2", 10), row("3", 99)}})
	mustBootstrap(t.Context(), t, s)
	gen := s.View().Gen
	if got := s.Visit(2, 7); got != 10 {
		t.Fatalf("Visit(2) = %d, want 10", got)
	}
	n := s.Prune(7, 50, gen)
	if n != 1 || s.Visit(1, 0) != 0 || s.Visit(2, 0) != 10 || s.Visit(3, 0) != 99 {
		t.Errorf("Prune removed %d; Visit = %d, %d, %d, want 1 removed and 0, 10, 99",
			n, s.Visit(1, 0), s.Visit(2, 0), s.Visit(3, 0))
	}
}

// Visits recorded before a rebuild belong to the discarded map, so a prune
// after it would delete every play the pass could not have visited.
func TestPrune_skipped_after_a_rebuild(t *testing.T) {
	s := newStore(&fakeSource{failPage: -1, rows: []plexapi.HistoryEntry{row("1", 10)}})
	mustBootstrap(t.Context(), t, s)
	gen := s.View().Gen
	mustBootstrap(t.Context(), t, s)
	if n := s.Prune(7, 50, gen); n != 0 || s.Visit(1, 0) != 10 {
		t.Errorf("Prune after a rebuild removed %d, Visit(1, 0) = %d, want 0 removed and 10", n, s.Visit(1, 0))
	}
}

func TestRead_paces_every_page_request(t *testing.T) {
	rows := make([]plexapi.HistoryEntry, 3*PageSize)
	for i := range rows {
		rows[i] = row("1", int64(i+1))
	}
	p := &countingPacer{}
	s := New(&fakeSource{failPage: -1, rows: rows}, p)
	mustBootstrap(t.Context(), t, s)
	if p.waits != 3 {
		t.Errorf("pacer waited %d times for a 3-page read, want 3", p.waits)
	}
}

func TestRun_failed_bootstrap_retries_after_backoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{failPage: 0, rows: []plexapi.HistoryEntry{row("1", 10)}}
		s := newStore(src)
		ctx, cancel := context.WithCancel(t.Context())
		rebuilt := make(chan struct{}, 1)
		var errs atomic.Int32
		go s.Run(ctx, func() { rebuilt <- struct{}{} }, func() { errs.Add(1) })
		synctest.Wait()
		if n := src.walks(); n != 1 {
			t.Fatalf("walks after start = %d, want 1", n)
		}
		time.Sleep(FailedRetryFloor - time.Second)
		synctest.Wait()
		if n := src.walks(); n != 1 {
			t.Fatalf("walks before the backoff ends = %d, want 1", n)
		}
		src.set(func(f *fakeSource) { f.failPage = -1 })
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-rebuilt:
		default:
			t.Fatalf("no rebuild signal after the retry (walks %d)", src.walks())
		}
		if n := errs.Load(); n != 1 {
			t.Errorf("error callbacks = %d, want 1", n)
		}
		cancel()
		synctest.Wait()
	})
}
