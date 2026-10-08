package server

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/libstats"
	"github.com/cplieger/plexapi/v2"
)

// Bounds of the catalog walk.
const (
	MaxWalkedLibraries = 32
	MaxWalkPages       = 1000
	WalkPageSize       = 500
	WalkDeadline       = 10 * time.Minute
	walkInterval       = time.Hour
	walkTypeMovie      = 1
)

var errWalkLimit = errors.New("library has more items than the walk limit")

// libWalk is one library's last successful walk. stamp is the history
// generation the watch figures were built from, 0 when history was not
// current for the whole walk.
type libWalk struct {
	lastSuccess time.Time
	stats       libstats.Stats
	duration    time.Duration
	stamp       uint64
	walked      bool
	failed      bool
}

type walkState struct {
	libs         map[string]*libWalk
	passDuration time.Duration
	pass         uint32
	passDone     bool
}

// walkableLibraries returns the video libraries the walk reads, in section
// id order, and how many past MaxWalkedLibraries it skips.
func walkableLibraries(libs []library.Library) (walk []library.Library, skipped int) {
	for _, l := range libs {
		switch l.Type {
		case library.TypeMovie, library.TypeShow, library.TypeHomevideo:
			walk = append(walk, l)
		}
	}
	slices.SortFunc(walk, func(a, b library.Library) int {
		x, _ := strconv.ParseUint(a.ID, 10, 64)
		y, _ := strconv.ParseUint(b.ID, 10, 64)
		return cmp.Compare(x, y)
	})
	if len(walk) > MaxWalkedLibraries {
		return walk[:MaxWalkedLibraries], len(walk) - MaxWalkedLibraries
	}
	return walk, 0
}

// RunLibraryWalkLoop walks every walkable library once the section list has
// been read, then again an hour after each pass started, or at once when
// the previous pass ran longer. A completed history rebuild (SignalWalk)
// re-walks the libraries whose figures predate it.
func (s *Server) RunLibraryWalkLoop(ctx context.Context) {
	if !s.waitForSections(ctx) {
		return
	}
	for {
		start := time.Now()
		s.WalkPass(ctx, false)
		if !s.waitNextPass(ctx, start.Add(walkInterval)) {
			return
		}
	}
}

// SignalWalk asks the walk loop to re-walk libraries built from an older
// history generation; repeated signals before it runs coalesce.
func (s *Server) SignalWalk() {
	select {
	case s.walkSignal <- struct{}{}:
	default:
	}
}

func (s *Server) waitForSections(ctx context.Context) bool {
	t := time.NewTicker(SessionPollInterval)
	defer t.Stop()
	for {
		s.mu.Lock()
		read := s.background.sectionsRead
		s.mu.Unlock()
		if read {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

func (s *Server) waitNextPass(ctx context.Context, at time.Time) bool {
	for {
		t := time.NewTimer(time.Until(at))
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
			return true
		case <-s.walkSignal:
			t.Stop()
			s.WalkPass(ctx, true)
		}
	}
}

// WalkPass walks the walkable libraries in order; staleOnly limits it to
// those whose watch figures are not from the current history generation.
// A full pass in which every library walked prunes the watched map of
// items no longer in any library, played before the pass started.
func (s *Server) WalkPass(ctx context.Context, staleOnly bool) {
	libs, skipped := walkableLibraries(s.SnapshotLibraries())
	start := time.Now()
	gen := s.historyView().Gen
	pass := uint32(0)
	if !staleOnly {
		s.mu.Lock()
		s.walks.pass++
		pass = s.walks.pass
		s.mu.Unlock()
	}
	allOK := true
	for _, lib := range libs {
		if staleOnly && s.stampOf(lib.ID) == gen {
			continue
		}
		ok := s.walkLibrary(ctx, &lib, pass)
		allOK = allOK && ok
		if ctx.Err() != nil {
			return
		}
	}
	if staleOnly {
		return
	}
	s.mu.Lock()
	s.walks.passDuration = time.Since(start)
	s.walks.passDone = true
	keep := make(map[string]bool, len(libs))
	for _, l := range libs {
		keep[l.ID] = true
	}
	for id := range s.walks.libs {
		if !keep[id] {
			delete(s.walks.libs, id)
		}
	}
	s.mu.Unlock()
	if allOK && skipped == 0 && s.History != nil {
		s.History.Prune(pass, start.Unix(), gen)
	}
}

// walkLibrary walks one library, marking each key it reads as seen by pass
// (0 marks nothing), and stores the figures when the walk completes.
func (s *Server) walkLibrary(ctx context.Context, lib *library.Library, pass uint32) bool {
	gen0 := s.stampableGen()
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, WalkDeadline)
	defer cancel()
	played := func(key uint64) int64 {
		if s.History == nil {
			return 0
		}
		return s.History.Visit(key, pass)
	}
	stats, err := libstats.Aggregate(s.pagedItems(wctx, lib), lib.Type == library.TypeShow, played, start)
	if err == nil {
		s.fillShowYears(wctx, lib.ID, &stats)
	}
	gen1 := s.stampableGen()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.walks.libs == nil {
		s.walks.libs = make(map[string]*libWalk)
	}
	w := s.walks.libs[lib.ID]
	if w == nil {
		w = &libWalk{}
		s.walks.libs[lib.ID] = w
	}
	if err != nil {
		if ctx.Err() == nil {
			if !w.failed {
				slog.Warn("library walk failed", "library_id", lib.ID, "error", err)
			}
			s.recordErrorLocked("library_walk")
			w.failed = true
		}
		return false
	}
	for i := range stats.Recent {
		stats.Recent[i].LibraryID, stats.Recent[i].LibraryType = lib.ID, lib.Type
	}
	for i := range stats.Largest {
		stats.Largest[i].LibraryID, stats.Largest[i].LibraryType = lib.ID, lib.Type
	}
	stamp := uint64(0)
	if gen0 != 0 && gen0 == gen1 {
		stamp = gen0
	}
	*w = libWalk{stats: stats, lastSuccess: time.Now(), duration: time.Since(start), stamp: stamp, walked: true}
	return true
}

// fillShowYears sets the year of each listed show from the library's last
// walk, or else from the show's metadata. A failed read leaves it unknown.
func (s *Server) fillShowYears(ctx context.Context, libID string, st *libstats.Stats) {
	years := s.knownShowYears(libID)
	for _, list := range [][]libstats.Item{st.Largest, st.Recent} {
		for i := range list {
			key := list[i].ShowKey
			if key == 0 {
				continue
			}
			y, ok := years[key]
			if !ok {
				y = s.showYear(ctx, key)
				years[key] = y
			}
			list[i].Year = y
		}
	}
}

func (s *Server) knownShowYears(libID string) map[uint64]int {
	years := make(map[uint64]int)
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.walks.libs[libID]; w != nil {
		for _, list := range [][]libstats.Item{w.stats.Largest, w.stats.Recent} {
			for _, it := range list {
				if it.ShowKey != 0 && it.Year != 0 {
					years[it.ShowKey] = it.Year
				}
			}
		}
	}
	return years
}

func (s *Server) showYear(ctx context.Context, key uint64) int {
	if s.wait(ctx) != nil {
		return 0
	}
	it, err := s.Client.Metadata(ctx, plexapi.RatingKey(strconv.FormatUint(key, 10)))
	if err != nil {
		if ctx.Err() == nil {
			s.RecordError("metadata_fetch")
		}
		return 0
	}
	return it.Year
}

func walkType(libType string) int {
	switch libType {
	case library.TypeMovie:
		return walkTypeMovie
	case library.TypeShow:
		return plexapi.MetadataTypeEpisode
	default:
		return 0
	}
}

// pagedItems walks one library under the shared pace and the item budget.
func (s *Server) pagedItems(ctx context.Context, lib *library.Library) iter.Seq2[plexapi.Item, error] {
	seq := s.Client.WalkSectionItems(ctx, plexapi.RatingKey(lib.ID), walkType(lib.Type), plexapi.Page{Size: WalkPageSize}, s.wait)
	return func(yield func(plexapi.Item, error) bool) {
		n := 0
		for it, err := range seq {
			switch {
			case err != nil:
				yield(plexapi.Item{}, err)
				return
			case n == MaxWalkPages*WalkPageSize:
				yield(plexapi.Item{}, errWalkLimit)
				return
			case !yield(it, nil):
				return
			}
			n++
		}
	}
}

func (s *Server) wait(ctx context.Context) error {
	if s.Pace == nil {
		return ctx.Err()
	}
	return s.Pace.Wait(ctx)
}

func (s *Server) historyView() history.View {
	if s.History == nil {
		return history.View{}
	}
	return s.History.View()
}

// stampableGen is the history generation a walk may be stamped with now,
// or 0 when history is not current.
func (s *Server) stampableGen() uint64 {
	if v := s.historyView(); v.Current {
		return v.Gen
	}
	return 0
}

func (s *Server) stampOf(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.walks.libs[id]; w != nil && w.walked {
		return w.stamp
	}
	return 0
}
