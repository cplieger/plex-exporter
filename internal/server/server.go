package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"golang.org/x/sync/errgroup"
)

// Server is the Plex orchestrator. Fields are exported so that the
// Describe/Collect code and tests can read and mutate them under
// mu without a wall of accessor methods. The whole internal/* tree is a
// single trust boundary.
type Server struct {
	LastItemsRefresh  time.Time
	ErrorCounts       map[string]float64
	Client            *plex.Client
	Sessions          *sessions.Tracker
	ID                string
	Name              string
	Version           string
	Platform          string
	PlatformVersion   string
	Libraries         []library.Library
	HostCPU           float64
	HostMem           float64
	TransmitBytes     float64
	LastBandwidthAt   int
	ActiveTranscodes  int
	mu                sync.Mutex
	refreshing        atomic.Bool
	HTTPReachable     bool
	SessionsReachable bool
	PlexPass          bool
}

// NewServer returns an initialised Server for the given Plex HTTP client.
// LastBandwidthAt is seeded to "now" so the first bandwidth refresh only
// picks up samples produced after startup, matching legacy behaviour.
func NewServer(client *plex.Client) *Server {
	return &Server{
		Client:          client,
		LastBandwidthAt: int(time.Now().Unix()),
		Sessions:        sessions.NewTracker(),
		ErrorCounts:     make(map[string]float64, len(metrics.ErrorTypes)),
	}
}

// RecordError increments the error counter for the given type. The type
// must be a member of metrics.ErrorTypes; unknown types are silently
// dropped to preserve the Prometheus cardinality bound.
func (s *Server) RecordError(typ string) {
	if !slices.Contains(metrics.ErrorTypes, typ) {
		return
	}
	s.mu.Lock()
	if s.ErrorCounts == nil {
		s.ErrorCounts = make(map[string]float64, len(metrics.ErrorTypes))
	}
	s.ErrorCounts[typ]++
	s.mu.Unlock()
}

// Refresh polls Plex for server identity, library list, host resources,
// and bandwidth. Intended to be called both from startup (to establish
// initial state) and from RunRefreshLoop on a ticker.
func (s *Server) Refresh(outerCtx context.Context) error {
	ctx, cancel := context.WithTimeout(outerCtx, 45*time.Second)
	defer cancel()

	// Server identity + library list.
	var providers plexapi.MC[plexapi.MediaProviderResponse]
	if err := s.Client.Get(ctx, "/media/providers?includeStorage=1", &providers); err != nil {
		return fmt.Errorf("fetching providers: %w", err)
	}

	s.mu.Lock()
	s.ID = providers.MediaContainer.MachineIdentifier
	s.Name = providers.MediaContainer.FriendlyName
	s.Version = providers.MediaContainer.Version

	// Build a lookup of existing item counts so they survive the rebuild.
	prevItems := make(map[string]int64, len(s.Libraries))
	for _, lib := range s.Libraries {
		if lib.ItemsCount > 0 {
			prevItems[lib.ID] = lib.ItemsCount
		}
	}

	s.Libraries = library.Build(providers.MediaContainer, prevItems)
	needItemsRefresh := time.Since(s.LastItemsRefresh) > 15*time.Minute
	s.mu.Unlock()

	// Server info from root endpoint.
	var info plexapi.MC[plexapi.ServerIdentity]
	if err := s.Client.Get(ctx, "/", &info); err != nil {
		return fmt.Errorf("fetching server info: %w", err)
	}
	s.mu.Lock()
	s.Platform = info.MediaContainer.Platform
	s.PlatformVersion = info.MediaContainer.PlatformVersion
	s.PlexPass = info.MediaContainer.MyPlexSubscription
	s.ActiveTranscodes = info.MediaContainer.TranscoderActiveVideoSessions
	s.mu.Unlock()

	// Library item counts (every 15 minutes).
	if needItemsRefresh {
		s.refreshLibraryItems(ctx)
		s.mu.Lock()
		s.LastItemsRefresh = time.Now()
		s.mu.Unlock()
	}

	// Resources + bandwidth (Plex Pass features, may 404).
	// Note: hostCpuUtilization and hostMemoryUtilization are returned as
	// percentages (0–100) by the Plex API. We divide by 100 to emit
	// ratios (0.0–1.0) matching our metric names (*_ratio).
	s.refreshResources(ctx)
	s.refreshBandwidth(ctx)
	return nil
}

// RunRefreshLoop invokes Refresh on a 5-second ticker until ctx is
// cancelled. On failure it flips HTTPReachable to false and records a
// "refresh" error; on recovery it logs a single info-level line to keep
// log volume bounded. If a previous Refresh is still in-flight the tick
// is skipped to prevent redundant concurrent HTTP calls.
func (s *Server) RunRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// Seed prevFailed from the reachability state established at startup so a recovery after a
	// degraded start logs "refresh recovered", symmetric with main()'s "starting in degraded
	// state" warning. A normal start has HTTPReachable=true here, so prevFailed=false and no
	// spurious recovery line fires on the first tick.
	s.mu.Lock()
	prevFailed := !s.HTTPReachable
	s.mu.Unlock()
	for {
		select {
		case <-ticker.C:
			if !s.refreshing.CompareAndSwap(false, true) {
				continue // previous Refresh still running, skip this tick
			}
			if err := s.Refresh(ctx); err != nil {
				s.SetHTTPReachable(false)
				s.RecordError("refresh")
				slog.Warn("refresh failed", "error", err)
				prevFailed = true
				s.refreshing.Store(false)
				continue
			}
			s.SetHTTPReachable(true)
			if prevFailed {
				slog.Info("refresh recovered")
				prevFailed = false
			}
			s.refreshing.Store(false)
		case <-ctx.Done():
			return
		}
	}
}

// SetHTTPReachable atomically sets the HTTPReachable flag.
func (s *Server) SetHTTPReachable(v bool) {
	s.mu.Lock()
	s.HTTPReachable = v
	s.mu.Unlock()
}

// SetSessionsReachable atomically sets the SessionsReachable flag.
func (s *Server) SetSessionsReachable(v bool) {
	s.mu.Lock()
	s.SessionsReachable = v
	s.mu.Unlock()
}

// SnapshotLibraries returns a copy of the current library list under the mutex.
func (s *Server) SnapshotLibraries() []library.Library {
	s.mu.Lock()
	libs := make([]library.Library, len(s.Libraries))
	copy(libs, s.Libraries)
	s.mu.Unlock()
	return libs
}

// Snapshot is an immutable view of Server captured under s.mu for
// metric emission. Keeping the snapshot/emit split tight keeps Collect's
// lock scope to a single block. PlexPass is stored as a string so the
// caller can emit it directly as a Prometheus label value.
type Snapshot struct {
	ErrorCounts       map[string]float64
	PlatformVersion   string
	Name              string
	ID                string
	Version           string
	Platform          string
	PlexPass          string
	Libraries         []library.Library
	HostCPU           float64
	HostMem           float64
	TransmitBytes     float64
	ActiveTranscodes  int
	HTTPReachable     float64
	SessionsReachable float64
	Retries           float64
}

// Snapshot returns a consistent point-in-time copy of the server's
// metric-visible state. Callers emit Prometheus metrics from the
// snapshot so Collect never holds s.mu across a channel send.
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		Name:             truncLabel(s.Name),
		ID:               truncLabel(s.ID),
		Version:          truncLabel(s.Version),
		Platform:         truncLabel(s.Platform),
		PlatformVersion:  truncLabel(s.PlatformVersion),
		PlexPass:         "false",
		HostCPU:          s.HostCPU,
		HostMem:          s.HostMem,
		TransmitBytes:    s.TransmitBytes,
		ActiveTranscodes: s.ActiveTranscodes,
		Libraries:        make([]library.Library, len(s.Libraries)),
		ErrorCounts:      make(map[string]float64, len(s.ErrorCounts)),
	}
	copy(snap.Libraries, s.Libraries)
	maps.Copy(snap.ErrorCounts, s.ErrorCounts)
	if s.PlexPass {
		snap.PlexPass = "true"
	}
	if s.HTTPReachable {
		snap.HTTPReachable = 1.0
	}
	if s.SessionsReachable {
		snap.SessionsReachable = 1.0
	}
	if s.Client != nil {
		snap.Retries = float64(s.Client.Retries())
	}
	return snap
}

func (s *Server) refreshLibraryItems(ctx context.Context) {
	s.mu.Lock()
	libs := make([]library.Library, len(s.Libraries))
	copy(libs, s.Libraries)
	s.mu.Unlock()

	// Bounded worker pool: min(4, len(libs)) goroutines fetch item counts
	// concurrently. Each goroutine writes to its own index so no mutex is
	// needed for the results slice.
	workers := min(4, len(libs))
	if workers == 0 {
		return
	}
	ch := make(chan int, len(libs))
	for i := range libs {
		ch <- i
	}
	close(ch)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range ch {
				s.fillItemCount(ctx, &libs[i])
			}
		}()
	}
	wg.Wait()

	s.writeBackItemCounts(libs)
}

// fillItemCount sets lb.ItemsCount from the first item-count query that
// returns a positive result for the library's type. When no query yields a
// positive count it logs at debug and leaves any existing count untouched.
func (s *Server) fillItemCount(ctx context.Context, lb *library.Library) {
	if _, err := strconv.Atoi(lb.ID); err != nil {
		slog.Warn("library item count: non-numeric section id, skipping",
			"id", lb.ID, "library", lb.Name)
		s.RecordError("library_items")
		return
	}
	for _, typ := range library.ItemCountTypes(lb.Type) {
		if count, ok := s.tryItemCount(ctx, lb.ID, typ); ok {
			lb.ItemsCount = count
			return
		}
	}
	if lb.ItemsCount == 0 {
		slog.Debug("library item count unavailable",
			"library", lb.Name, "id", lb.ID, "type", lb.Type)
	}
}

// writeBackItemCounts copies refreshed item counts back into s.Libraries
// under the lock, matching by index and ID so a library-list rebuild that
// raced with the fetch can't write a count onto the wrong section.
func (s *Server) writeBackItemCounts(libs []library.Library) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, lb := range libs {
		if i < len(s.Libraries) && s.Libraries[i].ID == lb.ID {
			s.Libraries[i].ItemsCount = lb.ItemsCount
		}
	}
}

// tryItemCount fetches the item count for a library section.
// Returns (count, true) on a positive result; (0, false) on error or zero.
// Zero is treated as "try the next fallback" to match pre-refactor behaviour
// where a zero result falls through to the default path for music libraries.
func (s *Server) tryItemCount(ctx context.Context, libID, typeParam string) (int64, bool) {
	path := "/library/sections/" + libID + "/all"
	if typeParam != "" {
		path += "?type=" + typeParam
	}
	count, err := s.Client.GetContainerSize(ctx, path)
	if err != nil {
		slog.Warn("library item count fetch failed",
			"library_id", libID, "type_param", typeParam, "error", err)
		s.RecordError("library_items")
		return 0, false
	}
	return count, count > 0
}

func (s *Server) refreshResources(ctx context.Context) {
	var resp plexapi.MC[struct {
		StatisticsResources []plexapi.StatisticsResource `json:"StatisticsResources"`
	}]
	if err := s.Client.Get(ctx, "/statistics/resources?timespan=6", &resp); err != nil {
		if ctx.Err() != nil {
			slog.Warn("resources fetch skipped, context deadline exceeded", "error", err)
		} else {
			slog.Debug("resources unavailable", "error", err)
		}
		return
	}
	stats := resp.MediaContainer.StatisticsResources
	if len(stats) == 0 {
		return
	}
	latest := stats[len(stats)-1]
	s.mu.Lock()
	s.HostCPU = latest.HostCPUUtil / 100
	s.HostMem = latest.HostMemUtil / 100
	s.mu.Unlock()
}

func (s *Server) refreshBandwidth(ctx context.Context) {
	var resp plexapi.MC[struct {
		StatisticsBandwidth []plexapi.StatisticsBandwidth `json:"StatisticsBandwidth"`
	}]
	if err := s.Client.Get(ctx, "/statistics/bandwidth?timespan=6", &resp); err != nil {
		if ctx.Err() != nil {
			slog.Warn("bandwidth fetch skipped, context deadline exceeded", "error", err)
		} else {
			slog.Debug("bandwidth unavailable", "error", err)
		}
		return
	}
	updates := resp.MediaContainer.StatisticsBandwidth
	slices.SortFunc(updates, func(a, b plexapi.StatisticsBandwidth) int { return a.At - b.At })

	s.mu.Lock()
	defer s.mu.Unlock()
	highest := s.LastBandwidthAt
	for _, u := range updates {
		if u.At > s.LastBandwidthAt {
			s.TransmitBytes += float64(u.Bytes)
			if u.At > highest {
				highest = u.At
			}
		}
	}
	s.LastBandwidthAt = highest
}

// SessionPollInterval is the interval between /status/sessions polls.
// Short enough (~5s) that the 60s tracker retention catches transient
// sessions between scrapes.
const SessionPollInterval = 5 * time.Second

// RunSessionPollLoop polls /status/sessions on a short interval, feeding
// the tracker with session state, transcode classification, and library
// labels. Replaces the former WebSocket event-driven architecture while
// keeping the tracker's accumulation/pruning/classification unchanged.
func (s *Server) RunSessionPollLoop(ctx context.Context) {
	ticker := time.NewTicker(SessionPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.RefreshSessions(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// sessionWork pairs a validated active session with its parsed playback
// state, carried through the metadata-fetch and tracker-apply passes.
type sessionWork struct {
	sess  *plexapi.SessionMetadata
	state sessions.State
}

// RefreshSessions fetches /status/sessions, applies each active session
// to the tracker, classifies transcode state inline (from the embedded
// TranscodeSession element), and fills library labels via
// /library/metadata/<ratingKey>.
func (s *Server) RefreshSessions(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var sessResp plexapi.MC[plexapi.MetadataListResponse]
	if err := s.Client.Get(fetchCtx, "/status/sessions", &sessResp); err != nil {
		slog.Warn("session poll: failed to fetch sessions", "error", err)
		s.RecordError("sessions_fetch")
		s.SetSessionsReachable(false)
		return
	}
	// Set reachable BEFORE the empty-sessions early return so a healthy
	// "no one watching" poll correctly reads 1 rather than going stale.
	s.SetSessionsReachable(true)

	// Reconcile sessions that vanished from this poll: Plex drops an ended
	// stream from /status/sessions rather than reporting it "stopped", so
	// mark any tracked session absent from this poll as stopped. That moves
	// it onto the 60s stopped-prune path (the documented retention) instead
	// of the 5-minute stale-orphan path, banking its final play time.
	present := make([]string, 0, len(sessResp.MediaContainer.Metadata))
	for i := range sessResp.MediaContainer.Metadata {
		if k := sessResp.MediaContainer.Metadata[i].SessionKey; k != "" {
			present = append(present, k)
		}
	}
	s.Sessions.MarkAbsentStopped(present)

	work := s.buildSessionWork(sessResp.MediaContainer.Metadata)
	if len(work) == 0 {
		return
	}

	mediaResults := s.fetchSessionMedia(fetchCtx, work)

	libs := s.SnapshotLibraries()
	for i := range work {
		s.applySessionUpdate(&work[i], mediaResults[i], libs)
	}
}

// buildSessionWork validates active sessions and pairs each kept session
// with its parsed playback state. Sessions with an empty key are skipped,
// and a non-numeric rating key is dropped (and recorded) so the later
// /library/metadata/<key> fetch is never built from unvalidated input.
func (s *Server) buildSessionWork(activeSessions []plexapi.SessionMetadata) []sessionWork {
	work := make([]sessionWork, 0, len(activeSessions))
	for i := range activeSessions {
		m := &activeSessions[i]
		if m.SessionKey == "" {
			continue
		}
		if _, err := strconv.Atoi(m.RatingKey); err != nil {
			slog.Warn("session poll: invalid rating key", "key", m.RatingKey)
			s.RecordError("invalid_rating_key")
			continue
		}
		work = append(work, sessionWork{sess: m, state: sessions.ParseState(m.Player.State)})
	}
	return work
}

// fetchSessionMedia fetches /library/metadata/<ratingKey> for each work item
// concurrently (at most 4 in flight) and returns the metadata keyed by work
// index. A fetch error or empty response leaves that index unset, so the
// caller still applies session state without library labels.
func (s *Server) fetchSessionMedia(ctx context.Context, work []sessionWork) []*plexapi.SessionMetadata {
	results := make([]*plexapi.SessionMetadata, len(work))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(min(4, len(work)))
	for i, w := range work {
		g.Go(func() error {
			var metaResp plexapi.MC[plexapi.MetadataListResponse]
			if err := s.Client.Get(gctx, "/library/metadata/"+w.sess.RatingKey, &metaResp); err != nil {
				slog.Warn("session poll: metadata fetch failed", "key", w.sess.RatingKey, "error", err)
				s.RecordError("metadata_fetch")
				return nil
			}
			if len(metaResp.MediaContainer.Metadata) == 0 {
				slog.Debug("session poll: empty metadata response", "key", w.sess.RatingKey)
				return nil
			}
			results[i] = &metaResp.MediaContainer.Metadata[0]
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// applySessionUpdate feeds one session's state into the tracker, attaches
// library labels when metadata was fetched, and classifies any transcode.
func (s *Server) applySessionUpdate(w *sessionWork, media *plexapi.SessionMetadata, libs []library.Library) {
	if media == nil {
		// Still update the tracker with session state even without library metadata.
		s.Sessions.Update(w.sess.SessionKey, w.state, w.sess, nil)
	} else {
		s.Sessions.Update(w.sess.SessionKey, w.state, w.sess, media)
		s.Sessions.UpdateLibraryLabels(w.sess.SessionKey, func(ss *sessions.Session) {
			fillSessionLibrary(ss, media, libs)
		})
	}
	s.classifyTranscode(w.sess)
}

// classifyTranscode derives transcode kind and subtitle action from the
// session's embedded TranscodeSession and writes them to the tracked
// session by SessionKey. No-op when the session carries no TranscodeSession.
func (s *Server) classifyTranscode(sess *plexapi.SessionMetadata) {
	ts := sess.TranscodeSession
	if ts == nil {
		return
	}
	kind := sessions.TranscodeKind(ts)
	subtitle := sessions.SubtitleAction(ts)
	s.Sessions.UpdateLibraryLabels(sess.SessionKey, func(ss *sessions.Session) {
		ss.TranscodeType = kind
		ss.SubtitleAction = subtitle
	})
}

// fillSessionLibrary populates library labels on ss when missing, using the
// provided library list matched by LibrarySectionID. No-op if ss already
// has a library name.
func fillSessionLibrary(ss *sessions.Session, media *plexapi.SessionMetadata, libs []library.Library) {
	if ss.LibName != "" {
		return
	}
	for _, lib := range libs {
		if lib.ID != media.LibrarySectionID.String() {
			continue
		}
		ss.LibName = lib.Name
		ss.LibID = lib.ID
		ss.LibType = lib.Type
		return
	}
}
