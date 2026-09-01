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

// Server is the Plex orchestrator. Fields are exported so Describe/Collect
// and tests can read and mutate them under mu without accessor methods.
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

// providersRefreshInterval is the /media/providers fetch cadence. That
// metadata only changes on upgrade/library-edit timescales, so refetching
// it on every 5s tick like identity/resources/bandwidth bought nothing.
const providersRefreshInterval = time.Minute

// Refresh polls Plex for server identity, library list, host resources,
// and bandwidth. Called from startup and from RunRefreshLoop on a ticker.
// The zero LastProvidersRefresh on the first call always fetches providers,
// so startup fail-fast classification still keys on that endpoint.
func (s *Server) Refresh(outerCtx context.Context) error {
	ctx, cancel := context.WithTimeout(outerCtx, 45*time.Second)
	defer cancel()

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

		// Presence in the map is the validity signal: only a count that was
		// actually read carries over, so an unread section stays unknown
		// rather than becoming a published zero.
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

	if needItemsRefresh {
		s.refreshLibraryItems(ctx)
		s.mu.Lock()
		s.LastItemsRefresh = time.Now()
		s.mu.Unlock()
	}

	// Resources + bandwidth are Plex Pass features and may 404.
	s.refreshResources(ctx)
	s.refreshBandwidth(ctx)
	return nil
}

// RunRefreshLoop invokes Refresh on a 5-second ticker until ctx is
// cancelled, skipping a tick if the previous Refresh is still in-flight.
func (s *Server) RunRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// Seed from startup's reachability state so a recovery after a degraded
	// start logs "refresh recovered" without a spurious line on tick one.
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

// SetHTTPReachable sets the HTTPReachable flag under lock.
func (s *Server) SetHTTPReachable(v bool) {
	s.mu.Lock()
	s.HTTPReachable = v
	s.mu.Unlock()
}

// SetSessionsReachable sets the SessionsReachable flag under lock.
func (s *Server) SetSessionsReachable(v bool) {
	s.mu.Lock()
	s.SessionsReachable = v
	s.mu.Unlock()
}

// SnapshotLibraries returns a copy of the current library list.
func (s *Server) SnapshotLibraries() []library.Library {
	s.mu.Lock()
	libs := make([]library.Library, len(s.Libraries))
	copy(libs, s.Libraries)
	s.mu.Unlock()
	return libs
}

// Snapshot is an immutable view of Server for metric emission, keeping
// Collect's lock scope to a single block. PlexPass is a string so the
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
// metric-visible state, so Collect never holds s.mu across a channel send.
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

	// Each goroutine writes to its own slice index, so no mutex is needed
	// for the results.
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

// fillItemCount sets lb.ItemsCount from the library type's item-count
// queries. A positive result wins immediately; a zero falls through to the
// next type in the chain (library.ItemCountTypes' artist-library fallback).
// An all-zero outcome is authoritative once any query answered — a library
// emptied to zero must publish 0. If no query answered, the previous count
// is left untouched so a fetch outage never reads as a collapse.
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

// writeBackItemCounts matches by index and ID so a library-list rebuild
// racing the fetch can't write a count onto the wrong section.
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

// tryItemCount fetches a library section's item count, reporting whether the
// query ANSWERED: (0, true) is a section that really holds no items, (0,
// false) is one whose count could not be read. A negative total is refused
// like a transport error rather than published as a gauge value.
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

	// Only samples newer than LastBandwidthAt are added (watermark
	// accumulation). This assumes the 5s refresh cadence never exceeds the
	// sample window the undocumented /statistics/bandwidth endpoint returns;
	// a missed window would undercount plex_transmit_bytes_total, which the
	// README already labels indicative-only.
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

// RunSessionPollLoop polls /status/sessions on SessionPollInterval, feeding
// the tracker with session state, transcode classification, and library
// labels, until ctx is cancelled.
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
// state.
type sessionWork struct {
	sess  *plexapi.Item
	state sessions.State
}

// RefreshSessions fetches /status/sessions, applies each active session to
// the tracker, classifies transcode state from the embedded
// TranscodeSession element, and fills library labels via
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

	// Plex drops an ended stream from /status/sessions rather than reporting
	// it "stopped", so mark any tracked session absent from this poll as
	// stopped: that moves it onto the 60s stopped-prune path instead of the
	// 5-minute stale-orphan path, banking its final play time.
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

// buildSessionWork skips sessions with an empty key and drops (recording)
// a non-numeric rating key, so the later /library/metadata/<key> fetch is
// never built from unvalidated input.
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
// (at most 4 in flight), keyed by work index. Every error, ErrNotFound
// included, leaves that index unset and records metadata_fetch; the caller
// still applies session state without library labels. A session whose
// tracked state already carries metadata for its current rating key is
// skipped (MediaMeta is immutable per key), so applySessionUpdate keeps the
// cached MediaMeta and one /library/metadata round-trip is saved per poll.
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
// session. No-op when the session carries no TranscodeSession.
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

// fillSessionLibrary populates library labels on ss when missing, matching
// media's LibrarySectionID against libs. No-op if ss already has a name.
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
