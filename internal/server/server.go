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

	"github.com/cplieger/plex-exporter/v2/internal/library"
	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"github.com/cplieger/plex-exporter/v2/internal/plex"
	"github.com/cplieger/plex-exporter/v2/internal/sessions"
	"github.com/cplieger/plexapi/v2"
	"golang.org/x/sync/errgroup"
)

// Server is the Plex orchestrator. Fields are exported so that the
// Describe/Collect code and tests can read and mutate them under
// mu without a wall of accessor methods. The whole internal/* tree is a
// single trust boundary.
type Server struct {
	LastItemsRefresh     time.Time
	LastProvidersRefresh time.Time
	ErrorCounts          map[string]float64
	Client               *plex.Client
	Sessions             *sessions.Tracker
	ID                   string
	Name                 string
	Version              string
	Platform             string
	PlatformVersion      string
	Libraries            []library.Library
	HostCPU              float64
	HostMem              float64
	TransmitBytes        float64
	LastBandwidthAt      int
	ActiveTranscodes     int
	mu                   sync.Mutex
	refreshing           atomic.Bool
	HTTPReachable        bool
	SessionsReachable    bool
	PlexPass             bool
}

// New returns an initialised Server for the given Plex HTTP client.
// LastBandwidthAt is seeded to "now" so the first bandwidth refresh only
// picks up samples produced after startup, matching legacy behaviour.
func New(client *plex.Client) *Server {
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

// providersRefreshInterval is the cadence for the /media/providers fetch.
// Providers metadata (server name/version, the library list and its
// duration/storage totals) changes on upgrade/library-edit timescales and
// the known scrapers read at 60s, so refetching it on every 5s tick bought
// nothing. Identity, resources, and bandwidth stay on the tick: they carry
// live state (active transcodes, host CPU/mem, bandwidth samples).
const providersRefreshInterval = time.Minute

// Refresh polls Plex for server identity, library list, host resources,
// and bandwidth. Intended to be called both from startup (to establish
// initial state) and from RunRefreshLoop on a ticker. The providers fetch
// runs at providersRefreshInterval rather than every call; the first call
// (zero LastProvidersRefresh) always fetches it, so startup fail-fast
// classification still keys on the providers endpoint.
func (s *Server) Refresh(outerCtx context.Context) error {
	ctx, cancel := context.WithTimeout(outerCtx, 45*time.Second)
	defer cancel()

	// Server identity + library list, at the slower providers cadence.
	s.mu.Lock()
	needProviders := time.Since(s.LastProvidersRefresh) > providersRefreshInterval
	s.mu.Unlock()
	if needProviders {
		providers, err := s.Client.Providers(ctx)
		if err != nil {
			return fmt.Errorf("fetching providers: %w", err)
		}

		s.mu.Lock()
		s.ID = providers.MachineIdentifier
		s.Name = providers.FriendlyName
		s.Version = providers.Version

		// Build a lookup of existing item counts so they survive the rebuild.
		// Presence in the map is the validity signal: only a count that was
		// actually read carries over, so a section whose count is unknown stays
		// unknown rather than becoming a published zero.
		prevItems := make(map[string]int64, len(s.Libraries))
		for _, lib := range s.Libraries {
			if lib.ItemsKnown {
				prevItems[lib.ID] = lib.ItemsCount
			}
		}

		s.Libraries = library.Build(providers, prevItems)
		s.LastProvidersRefresh = time.Now()
		s.mu.Unlock()
	}

	s.mu.Lock()
	needItemsRefresh := time.Since(s.LastItemsRefresh) > 15*time.Minute
	s.mu.Unlock()

	// Server info from root endpoint.
	info, err := s.Client.Identity(ctx)
	if err != nil {
		return fmt.Errorf("fetching server info: %w", err)
	}
	s.mu.Lock()
	s.Platform = info.Platform
	s.PlatformVersion = info.PlatformVersion
	s.PlexPass = info.MyPlexSubscription
	s.ActiveTranscodes = info.TranscoderActiveVideoSessions
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
	for range workers {
		wg.Go(func() {
			for i := range ch {
				s.fillItemCount(ctx, &libs[i])
			}
		})
	}
	wg.Wait()

	s.writeBackItemCounts(libs)
}

// fillItemCount sets lb.ItemsCount from the library type's item-count queries,
// marking it known. A positive result wins immediately; a zero falls through to
// the next type in the chain, which is the artist-library behaviour
// library.ItemCountTypes exists for. Once the chain is exhausted, an all-zero
// outcome is authoritative as long as at least one query answered — a library
// emptied to exactly zero items must publish 0, not keep its last count. When
// no query answered at all it logs at debug and leaves the previous count and
// its known-ness untouched, so a fetch outage never reads as a collapse.
func (s *Server) fillItemCount(ctx context.Context, lb *library.Library) {
	if _, err := strconv.Atoi(lb.ID); err != nil {
		slog.Warn("library item count: non-numeric section id, skipping",
			"id", lb.ID, "library", lb.Name)
		s.RecordError("library_items")
		return
	}
	var (
		count    int64
		answered bool
	)
	for _, typ := range library.ItemCountTypes(lb.Type) {
		c, ok := s.tryItemCount(ctx, lb.ID, typ)
		if !ok {
			continue
		}
		answered = true
		count = c
		if c > 0 {
			break
		}
	}
	if !answered {
		slog.Debug("library item count unavailable",
			"library", lb.Name, "id", lb.ID, "type", lb.Type)
		return
	}
	lb.ItemsCount = count
	lb.ItemsKnown = true
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
			s.Libraries[i].ItemsKnown = lb.ItemsKnown
		}
	}
}

// tryItemCount fetches the item count for a library section, reporting whether
// the query ANSWERED — (0, true) is a section that really holds no items and
// (0, false) is a section whose count could not be read, which the caller must
// keep apart. A negative total cannot describe a collection, so it is refused
// like a transport error rather than published as a gauge value. The section
// path and container params are built by the plexapi library (metadataType 0 =
// unfiltered).
func (s *Server) tryItemCount(ctx context.Context, libID string, metadataType int) (count int64, answered bool) {
	count, err := s.Client.CountSectionItems(ctx, plexapi.RatingKey(libID), metadataType)
	if err != nil {
		slog.Warn("library item count fetch failed",
			"library_id", libID, "type_param", metadataType, "error", err)
		s.RecordError("library_items")
		return 0, false
	}
	if count < 0 {
		slog.Warn("library item count: negative total reported, ignoring",
			"library_id", libID, "type_param", metadataType, "count", count)
		s.RecordError("library_items")
		return 0, false
	}
	return count, true
}

func (s *Server) refreshResources(ctx context.Context) {
	stats, err := s.Client.StatisticsResources(ctx, 6)
	if err != nil {
		if ctx.Err() != nil {
			slog.Warn("resources fetch skipped, context deadline exceeded", "error", err)
		} else {
			slog.Debug("resources unavailable", "error", err)
		}
		return
	}
	if len(stats) == 0 {
		return
	}
	latest := stats[len(stats)-1]
	s.mu.Lock()
	s.HostCPU = latest.HostCPUUtilization / 100
	s.HostMem = latest.HostMemoryUtilization / 100
	s.mu.Unlock()
}

func (s *Server) refreshBandwidth(ctx context.Context) {
	samples, err := s.Client.StatisticsBandwidth(ctx, 6)
	if err != nil {
		if ctx.Err() != nil {
			slog.Warn("bandwidth fetch skipped, context deadline exceeded", "error", err)
		} else {
			slog.Debug("bandwidth unavailable", "error", err)
		}
		return
	}
	updates := samples
	slices.SortFunc(updates, func(a, b plexapi.StatisticsBandwidth) int { return a.At - b.At })

	// Watermark accumulation: only samples newer than LastBandwidthAt are
	// added. Correctness assumes the 5s refresh cadence never exceeds the
	// sample window the undocumented /statistics/bandwidth endpoint returns
	// (unverifiable without a Plex Pass instance); a missed window would
	// undercount plex_transmit_bytes_total, which the README already labels
	// indicative-only (reset on restart).
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
	sess  *plexapi.Item
	state sessions.State
}

// RefreshSessions fetches /status/sessions, applies each active session
// to the tracker, classifies transcode state inline (from the embedded
// TranscodeSession element), and fills library labels via
// /library/metadata/<ratingKey>.
func (s *Server) RefreshSessions(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	activeSessions, err := s.Client.Sessions(fetchCtx)
	if err != nil {
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
	present := make([]string, 0, len(activeSessions))
	for i := range activeSessions {
		if k := activeSessions[i].SessionKey; k != "" {
			present = append(present, k)
		}
	}
	s.Sessions.MarkAbsentStopped(present)

	work := s.buildSessionWork(activeSessions)
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
func (s *Server) buildSessionWork(activeSessions []plexapi.Item) []sessionWork {
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
		playerState := ""
		if m.Player != nil {
			playerState = m.Player.State
		}
		work = append(work, sessionWork{sess: m, state: sessions.ParseState(playerState)})
	}
	return work
}

// fetchSessionMedia fetches each work item's library metadata concurrently
// (at most 4 in flight) and returns it keyed by work index. Every error
// leaves that index unset and records metadata_fetch, ErrNotFound included:
// plexapi maps a 404 and an empty container onto it alike, and a live
// session whose item cannot be fetched is the case worth alerting on. The
// caller still applies session state without library labels. A session
// whose tracked state already carries metadata for its current rating key
// is skipped (MediaMeta is immutable per key): the nil result makes
// applySessionUpdate keep the cached MediaMeta, saving one
// /library/metadata round-trip per session per poll.
func (s *Server) fetchSessionMedia(ctx context.Context, work []sessionWork) []*plexapi.Item {
	results := make([]*plexapi.Item, len(work))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(min(4, len(work)))
	for i, w := range work {
		if s.Sessions.MediaResolved(w.sess.SessionKey, w.sess.RatingKey) {
			continue
		}
		g.Go(func() error {
			item, err := s.Client.Metadata(gctx, plexapi.RatingKey(w.sess.RatingKey))
			if err != nil {
				slog.Warn("session poll: metadata fetch failed", "key", w.sess.RatingKey, "error", err)
				s.RecordError("metadata_fetch")
				return nil
			}
			results[i] = item
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// applySessionUpdate feeds one session's state into the tracker, attaches
// library labels when metadata was fetched, and classifies any transcode.
func (s *Server) applySessionUpdate(w *sessionWork, media *plexapi.Item, libs []library.Library) {
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
func (s *Server) classifyTranscode(sess *plexapi.Item) {
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
func fillSessionLibrary(ss *sessions.Session, media *plexapi.Item, libs []library.Library) {
	if ss.LibName != "" {
		return
	}
	for _, lib := range libs {
		if lib.ID != strconv.Itoa(int(media.LibrarySectionID)) {
			continue
		}
		ss.LibName = lib.Name
		ss.LibID = lib.ID
		ss.LibType = lib.Type
		return
	}
}
