package server

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plexapi/v2"
)

// Cadences and the per-call deadline of the reads Refresh adds on its tick.
const (
	sectionsInterval   = time.Minute
	activitiesInterval = 30 * time.Second
	updaterInterval    = 15 * time.Minute
)

// backgroundDeadline bounds each of those reads; a var so a test can
// shorten the hang case.
var backgroundDeadline = 10 * time.Second

// activitiesStaleAfter is how long the last good activities answer is
// published as current; it outlasts one failed read.
const activitiesStaleAfter = 3 * activitiesInterval

// section is one /library/sections entry; scannedAt is 0 when unknown.
type section struct {
	id, typ   string
	scannedAt int64
}

// activityKey identifies one plex_server_activity_progress_ratio series.
type activityKey struct{ typ, libType, libID string }

// updateState is the decided update status; known is false while it reads
// Unknown.
type updateState struct {
	state     string
	checkedAt int64
	available float64
	known     bool
}

type backgroundState struct {
	lastSections, lastActivities, lastUpdater time.Time
	// activitiesAt is when activities was last read successfully.
	activitiesAt time.Time
	sections     []section
	activities   map[activityKey]float64
	update       updateState
	sectionsRead bool
}

// refreshBackground runs whichever of the sections, activities and update
// reads are due. Each has its own deadline and error type and never fails
// the caller's refresh.
func (s *Server) refreshBackground(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	b := &s.background
	due := func(last time.Time, every time.Duration) bool { return now.Sub(last) >= every }
	needSections := due(b.lastSections, sectionsInterval)
	needActivities := due(b.lastActivities, activitiesInterval)
	needUpdater := due(b.lastUpdater, updaterInterval)
	s.mu.Unlock()

	if needSections {
		s.refreshSections(ctx)
	}
	if needActivities {
		s.refreshActivities(ctx)
	}
	if needUpdater {
		s.refreshUpdater(ctx)
	}
}

func (s *Server) paced(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	ctx, cancel := context.WithTimeout(ctx, backgroundDeadline)
	if s.Pace != nil && s.Pace.Wait(ctx) != nil {
		cancel()
		return nil, nil, false
	}
	return ctx, cancel, true
}

func (s *Server) refreshSections(ctx context.Context) {
	ctx, cancel, ok := s.paced(ctx)
	if !ok {
		return
	}
	defer cancel()
	secs, err := s.Client.Sections(ctx)
	s.mu.Lock()
	s.background.lastSections = time.Now()
	s.mu.Unlock()
	if err != nil {
		slog.Debug("library sections unavailable", "error", err)
		s.RecordError("sections_fetch")
		return
	}
	out := make([]section, 0, min(len(secs), library.MaxLibraries))
	for _, sec := range secs {
		if len(out) == library.MaxLibraries || !library.IsType(sec.Type) || !numericID(sec.Key) {
			continue
		}
		var scanned int64
		if sec.ScannedAt != nil && *sec.ScannedAt > 0 {
			scanned = int64(*sec.ScannedAt)
		}
		out = append(out, section{id: sec.Key, typ: sec.Type, scannedAt: scanned})
	}
	s.mu.Lock()
	s.background.sections = out
	s.background.sectionsRead = true
	s.mu.Unlock()
}

func (s *Server) refreshActivities(ctx context.Context) {
	ctx, cancel, ok := s.paced(ctx)
	if !ok {
		return
	}
	defer cancel()
	acts, err := s.Client.Activities(ctx)
	s.mu.Lock()
	s.background.lastActivities = time.Now()
	secs := s.background.sections
	s.mu.Unlock()
	if err != nil {
		slog.Debug("activities unavailable", "error", err)
		s.RecordError("activities_fetch")
		return
	}
	merged := mergeActivities(acts, secs)
	s.mu.Lock()
	s.background.activities = merged
	s.background.activitiesAt = time.Now()
	s.mu.Unlock()
}

// mergeActivities maps Plex activities onto the bounded series set: a type
// from a closed list, and a section id only when it is a known library.
// Two activities on one series keep the larger progress.
func mergeActivities(acts []plexapi.Activity, secs []section) map[activityKey]float64 {
	types := make(map[string]string, len(secs))
	for _, sec := range secs {
		types[sec.id] = sec.typ
	}
	out := make(map[activityKey]float64)
	for _, a := range acts {
		ratio, ok := progressRatio(a.Progress)
		if !ok {
			slog.Debug("activity dropped", "reason", "progress missing or out of range")
			continue
		}
		key := activityKey{typ: activityType(a.Type)}
		if typ, known := types[a.LibrarySectionID]; known {
			key.libType, key.libID = typ, a.LibrarySectionID
		}
		if prev, seen := out[key]; !seen || ratio > prev {
			out[key] = ratio
		}
	}
	return out
}

// progressRatio maps Plex's percentage (-1 for no estimate) to 0-1 or -1.
func progressRatio(p *float64) (float64, bool) {
	switch {
	case p == nil || math.IsNaN(*p) || *p < -1 || *p > 100:
		return 0, false
	case *p < 0:
		return -1, true
	default:
		return *p / 100, true
	}
}

// activityType buckets Plex's dotted activity type names.
func activityType(t string) string {
	t = strings.ToLower(t)
	switch {
	case strings.HasPrefix(t, "library.update.section"), strings.HasPrefix(t, "library.scan"):
		return "scan"
	case strings.HasPrefix(t, "media.generate"), strings.HasPrefix(t, "media.analyze"):
		return "analysis"
	case strings.HasPrefix(t, "library.refresh"), strings.HasPrefix(t, "library.update.item"),
		strings.HasPrefix(t, "agent."), strings.HasPrefix(t, "metadata."):
		return "metadata"
	case strings.HasPrefix(t, "butler."), strings.HasPrefix(t, "database."), strings.HasPrefix(t, "library.optimize"),
		strings.HasPrefix(t, "library.clean"):
		return "maintenance"
	case strings.HasPrefix(t, "transcode."), strings.HasPrefix(t, "sync."), strings.HasPrefix(t, "download."):
		return "streaming"
	default:
		return metrics.FallbackOther
	}
}

func (s *Server) refreshUpdater(ctx context.Context) {
	ctx, cancel, ok := s.paced(ctx)
	if !ok {
		return
	}
	defer cancel()
	st, err := s.Client.UpdateStatus(ctx)
	if err != nil && !errors.Is(err, plex.ErrNotFound) {
		slog.Debug("update status unavailable", "error", err)
		s.RecordError("updater_fetch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background.lastUpdater = time.Now()
	s.background.update = updateState{}
	if err == nil {
		s.background.update = decideUpdate(st, s.Version)
	}
}

// Update states that mean the running server is behind the release.
var behindStates = map[string]bool{
	"available": true, "notify": true, "downloading": true, "downloaded": true,
	"tonight": true, "installing": true, "error": true, "skipped": true,
}

// decideUpdate maps an update check to Up to date (0), Update available
// (1), or unknown. A release counts only when its version differs from the
// running one, so an installed build Plex keeps listing reads up to date.
func decideUpdate(st *plexapi.UpdateStatus, running string) updateState {
	running = strings.TrimSpace(running)
	if st == nil || st.CheckedAt == nil || *st.CheckedAt <= 0 || st.Status == nil || *st.Status != 0 || running == "" {
		return updateState{}
	}
	out := updateState{known: true, checkedAt: int64(*st.CheckedAt), state: "none"}
	for i, r := range st.Releases {
		state := strings.ToLower(strings.TrimSpace(r.State))
		v := 0.0
		if behindStates[state] && strings.TrimSpace(r.Version) != running {
			v = 1
		}
		if !behindStates[state] && state != "done" {
			state = metrics.FallbackOther
		}
		if i == 0 || v > out.available {
			out.available, out.state = v, state
		}
	}
	return out
}

func numericID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
