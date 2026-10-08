// Package history keeps the newest play of every item by any account, read
// from Plex's watch history, and says whether that record is current enough
// to publish figures built from it.
package history

import (
	"context"
	"errors"
	"hash/maphash"
	"iter"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/plexapi/v2"
)

// Fixed bounds of the history reads.
const (
	PageSize            = 500
	MaxPages            = 500
	MaxIncrementalPages = 20
	MaxCatchupPages     = 200
	BootstrapDeadline   = 20 * time.Minute
	StaleAfter          = time.Hour
	IncrementalInterval = 5 * time.Minute
	CatchupInterval     = 24 * time.Hour
	CatchupWindow       = 30 * 24 * time.Hour
	RetryInterval       = 24 * time.Hour
	FailedRetryFloor    = 5 * time.Minute
	FailedRetryCeiling  = 6 * time.Hour
	incrementalOverlap  = 300
	// MaxCountedRows caps each of the unmatched and deleted-item row
	// counts. A full read holds at most this many rows, so its counts are
	// exact; later reads saturate at it.
	MaxCountedRows = MaxPages * PageSize
)

// Status is the outcome of the last full history read.
type Status string

// Status values published on plex_history_read_status.
const (
	StatusComplete  Status = "complete"
	StatusOverLimit Status = "over_limit"
	StatusFailed    Status = "failed"
)

// Statuses lists every Status in display order.
var Statuses = []Status{StatusComplete, StatusOverLimit, StatusFailed}

// Source pages through watch history, calling wait before every page
// request; *plexapi.Client satisfies it.
type Source interface {
	WalkHistory(ctx context.Context, sinceUnix int64, page plexapi.Page, wait func(context.Context) error) iter.Seq2[plexapi.HistoryEntry, error]
}

// Pacer delays a background request until the shared budget allows it.
type Pacer interface {
	Wait(ctx context.Context) error
}

// entry is one item's newest play, as Unix seconds, and the last walk pass
// that saw the item. Both fit 32 bits until 2106 and keep the map small.
type entry struct{ at, pass uint32 }

func merge(m map[uint64]entry, key uint64, viewedAt int64) {
	e := m[key]
	e.at = max(e.at, uint32(min(max(viewedAt, 0), math.MaxUint32)))
	m[key] = e
}

// Store is the watched map and its read state. The zero value is not
// usable; call New.
type Store struct {
	lastSuccess time.Time
	watched     map[uint64]entry
	unmatched   map[uint64]struct{}
	deleted     map[uint64]struct{}
	src         Source
	pace        Pacer
	now         func() time.Time
	status      Status
	seed        maphash.Seed
	gen         uint64
	cursor      int64
	drain       drain
	mu          sync.Mutex
}

// drain continues a capped increment: the next read repeats since and
// starts at row offset, until a read ends under the cap. A timestamp alone
// cannot resume, because the rows past the cap may share the last second
// read. offset 0 means no capped window is open.
type drain struct {
	since  int64
	offset int
}

// New returns an empty Store reading src under pace.
func New(src Source, pace Pacer) *Store {
	return &Store{src: src, pace: pace, now: time.Now, seed: maphash.MakeSeed()}
}

// View is a consistent snapshot of the read state. Unmatched rows could be
// plays of a current item; Deleted rows are plays of items Plex no longer
// has, so they never stand for a current item.
type View struct {
	LastSuccess time.Time
	Status      Status
	Gen         uint64
	Unmatched   int
	Deleted     int
	Current     bool
}

// View reports the state at now. Current means the last full read completed
// and some read succeeded within StaleAfter. Gen changes each time a full
// read ends, so equal Gen values bracket a span in which the watched map was
// neither rebuilt nor discarded.
func (s *Store) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return View{
		Status:      s.status,
		Gen:         s.gen,
		LastSuccess: s.lastSuccess,
		Unmatched:   len(s.unmatched),
		Deleted:     len(s.deleted),
		Current:     s.status == StatusComplete && s.now().Sub(s.lastSuccess) < StaleAfter,
	}
}

// Visit returns the newest recorded play of key, or 0, and records that walk
// pass saw key in the catalog; pass 0 records nothing.
func (s *Store) Visit(key uint64, pass uint32) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.watched[key]
	if ok && pass != 0 {
		e.pass = pass
		s.watched[key] = e
	}
	return int64(e.at)
}

// Prune deletes every key that walk pass did not visit and whose newest
// play is before cutoff, the pass start. It does nothing when the map was
// rebuilt or discarded since gen, because the visits were recorded in a
// map that no longer exists.
func (s *Store) Prune(pass uint32, cutoff int64, gen uint64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen || pass == 0 {
		return 0
	}
	n := 0
	for k, e := range s.watched {
		if e.pass != pass && int64(e.at) < cutoff {
			delete(s.watched, k)
			n++
		}
	}
	return n
}

var errOverLimit = errors.New("history over the read limit")

// read pages from since, starting at row offset start, pacing every page
// request, handing at most maxPages*PageSize rows to visit. It returns the
// rows read and reports errOverLimit when a row past that budget arrives.
func (s *Store) read(ctx context.Context, since int64, start, maxPages int, visit func(plexapi.HistoryEntry)) (int, error) {
	rows := 0
	for e, err := range s.src.WalkHistory(ctx, since, plexapi.Page{Start: start, Size: PageSize}, s.pace.Wait) {
		if err != nil {
			return rows, err
		}
		if rows == maxPages*PageSize {
			return rows, errOverLimit
		}
		visit(e)
		rows++
	}
	return rows, nil
}

// rowKind is how a history row is used.
type rowKind uint8

const (
	rowPlay rowKind = iota
	rowDeleted
	rowUnmatched
)

// classify sorts a row. Plex omits the item key on a play of an item it has
// since deleted, so such a row belongs to no current item. Any other row
// without a numeric key or a play time is unmatched: it may be a play of a
// current item, which would then read as never played.
func classify(e *plexapi.HistoryEntry) (uint64, rowKind) {
	if e.RatingKey == "" {
		return 0, rowDeleted
	}
	key, err := strconv.ParseUint(e.RatingKey, 10, 64)
	if err != nil || e.ViewedAt <= 0 {
		return 0, rowUnmatched
	}
	return key, rowPlay
}

// fold writes a play to watched and counts any other row in its set.
func (s *Store) fold(watched map[uint64]entry, unmatched, deleted map[uint64]struct{}, e *plexapi.HistoryEntry) {
	switch key, kind := classify(e); kind {
	case rowPlay:
		merge(watched, key, e.ViewedAt)
	case rowDeleted:
		s.count(deleted, e)
	default:
		s.count(unmatched, e)
	}
}

// identity hashes a row to a fixed-size key, so a long untrusted key costs
// a row set 8 bytes. A row without a history key is identified by its item
// key, play time and account.
func (s *Store) identity(e *plexapi.HistoryEntry) uint64 {
	if e.HistoryKey != "" {
		return maphash.String(s.seed, "h"+e.HistoryKey)
	}
	b := strconv.AppendInt([]byte{'r'}, e.ViewedAt, 10)
	b = strconv.AppendInt(append(b, '|'), e.AccountID, 10)
	return maphash.Bytes(s.seed, append(append(b, '|'), e.RatingKey...))
}

func (s *Store) count(set map[uint64]struct{}, e *plexapi.HistoryEntry) {
	if len(set) < MaxCountedRows {
		set[s.identity(e)] = struct{}{}
	}
}

// Bootstrap rebuilds the watched map from the whole history. A failure or
// a read past the limits discards the map, so nothing is built from part
// of the history.
func (s *Store) Bootstrap(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, BootstrapDeadline)
	defer cancel()
	watched := make(map[uint64]entry)
	unmatched, deleted := make(map[uint64]struct{}), make(map[uint64]struct{})
	var cursor int64
	_, err := s.read(ctx, 0, 0, MaxPages, func(e plexapi.HistoryEntry) {
		cursor = max(cursor, e.ViewedAt)
		s.fold(watched, unmatched, deleted, &e)
	})

	status := StatusComplete
	switch {
	case errors.Is(err, errOverLimit) || errors.Is(err, context.DeadlineExceeded):
		status = StatusOverLimit
	case err != nil:
		status = StatusFailed
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.status = status
	s.drain = drain{}
	if status != StatusComplete {
		s.watched, s.unmatched, s.deleted, s.cursor = nil, nil, nil, 0
		return status
	}
	s.watched, s.unmatched, s.deleted, s.cursor = watched, unmatched, deleted, cursor
	s.lastSuccess = s.now()
	return status
}

// Incremental reads plays since the cursor, with an overlap that re-reads
// the last few minutes. A failure keeps the map, the cursor and any open
// drain; reaching the page cap opens or extends a drain, so the next call
// reads on from the row after the last one read.
func (s *Store) Incremental(ctx context.Context) error {
	s.mu.Lock()
	if s.status != StatusComplete {
		s.mu.Unlock()
		return nil
	}
	since, start := max(s.cursor-incrementalOverlap, 0), 0
	if s.drain.offset > 0 {
		since, start = s.drain.since, s.drain.offset
	}
	s.mu.Unlock()
	cursor, rows, err := s.merge(ctx, since, start, MaxIncrementalPages)
	capped := errors.Is(err, errOverLimit)
	if err != nil && !capped {
		return err
	}
	s.mu.Lock()
	s.cursor = max(s.cursor, cursor)
	s.drain = drain{}
	if capped {
		s.drain = drain{since: since, offset: start + rows}
	}
	s.lastSuccess = s.now()
	s.mu.Unlock()
	return nil
}

// Catchup re-reads the last 30 days for plays a client reported late. It
// never discards state; a failure or the page cap is returned for logging.
func (s *Store) Catchup(ctx context.Context) error {
	s.mu.Lock()
	ok := s.status == StatusComplete
	s.mu.Unlock()
	if !ok {
		return nil
	}
	_, _, err := s.merge(ctx, s.now().Add(-CatchupWindow).Unix(), 0, MaxCatchupPages)
	return err
}

// merge folds rows into the current map; max makes a re-read idempotent.
// It returns the newest play time of every row read, unusable ones
// included, and the row count.
func (s *Store) merge(ctx context.Context, since int64, start, maxPages int) (cursor int64, rows int, err error) {
	rows, err = s.read(ctx, since, start, maxPages, func(e plexapi.HistoryEntry) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.watched == nil {
			return
		}
		cursor = max(cursor, e.ViewedAt)
		s.fold(s.watched, s.unmatched, s.deleted, &e)
	})
	return cursor, rows, err
}

// Run bootstraps, then reads increments every IncrementalInterval and the
// last 30 days every CatchupInterval, until ctx ends. After each completed
// bootstrap it calls onRebuilt. A failed bootstrap retries after
// FailedRetryFloor, doubling to FailedRetryCeiling; one over the limits retries
// after RetryInterval.
func (s *Store) Run(ctx context.Context, onRebuilt, onError func()) {
	backoff := FailedRetryFloor
	for {
		wait := s.bootstrapOnce(ctx, &backoff, onRebuilt, onError)
		if wait == 0 {
			s.follow(ctx, onError)
			return
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (s *Store) bootstrapOnce(ctx context.Context, backoff *time.Duration, onRebuilt, onError func()) time.Duration {
	switch st := s.Bootstrap(ctx); {
	case ctx.Err() != nil:
		return 0
	case st == StatusComplete:
		v := s.View()
		if v.Unmatched > 0 {
			slog.Warn("history rows unmatched", "rows", v.Unmatched)
		}
		onRebuilt()
		return 0
	case st == StatusOverLimit:
		slog.Warn("history over limit", "rows", MaxPages*PageSize)
		return RetryInterval
	default:
		onError()
		wait := *backoff
		*backoff = min(*backoff*2, FailedRetryCeiling)
		slog.Warn("history bootstrap failed", "status", string(st), "retry_in", wait.String())
		return wait
	}
}

func (s *Store) follow(ctx context.Context, onError func()) {
	if ctx.Err() != nil {
		return
	}
	inc := time.NewTicker(IncrementalInterval)
	defer inc.Stop()
	daily := time.NewTicker(CatchupInterval)
	defer daily.Stop()
	unmatched := s.View().Unmatched
	for {
		select {
		case <-ctx.Done():
			return
		case <-inc.C:
			unmatched = s.incrementalTick(ctx, unmatched, onError)
		case <-daily.C:
			if err := s.Catchup(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("history catch-up incomplete", "error", err)
			}
		}
	}
}

// incrementalTick runs one increment and warns once when unmatched rows
// first appear; it returns the unmatched count for the next tick.
func (s *Store) incrementalTick(ctx context.Context, unmatched int, onError func()) int {
	if err := s.Incremental(ctx); err != nil && ctx.Err() == nil {
		onError()
		slog.Debug("history read failed", "error", err)
	}
	n := s.View().Unmatched
	if n > 0 && unmatched == 0 {
		slog.Warn("history rows unmatched", "rows", n)
	}
	return n
}
