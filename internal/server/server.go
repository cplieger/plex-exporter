package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
)

// Server is the Plex orchestrator. Fields are exported so that package
// main (Describe/Collect/websocket code still living there this step)
// and tests can read and mutate them under mu without a wall of
// accessor methods. The whole internal/* tree is a single trust
// boundary.
type Server struct {
	LastItemsRefresh time.Time
	ErrorCounts      map[string]float64
	Client           *plex.Client
	Sessions         *sessions.Tracker
	ID               string
	Name             string
	Version          string
	Platform         string
	PlatformVersion  string
	Libraries        []library.Library
	HostCPU          float64
	HostMem          float64
	TransmitBytes    float64
	LastBandwidthAt  int
	ActiveTranscodes int
	mu               sync.Mutex
	refreshing       atomic.Bool
	HTTPReachable    bool
	PlexPass         bool
	WSConnected      bool
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
	var prevFailed bool
	for {
		select {
		case <-ticker.C:
			if !s.refreshing.CompareAndSwap(false, true) {
				continue // previous Refresh still running, skip this tick
			}
			if err := s.Refresh(ctx); err != nil {
				s.mu.Lock()
				s.HTTPReachable = false
				s.mu.Unlock()
				s.RecordError("refresh")
				slog.Warn("refresh failed", "error", err)
				prevFailed = true
				s.refreshing.Store(false)
				continue
			}
			s.mu.Lock()
			s.HTTPReachable = true
			s.mu.Unlock()
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

// SetWSConnected atomically sets the WSConnected flag.
func (s *Server) SetWSConnected(v bool) {
	s.mu.Lock()
	s.WSConnected = v
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

// Snapshot is an immutable view of Server captured under s.Mu for
// metric emission. Keeping the snapshot/emit split tight keeps Collect's
// lock scope to a single block. PlexPass is stored as a string so the
// caller can emit it directly as a Prometheus label value.
type Snapshot struct {
	ErrorCounts      map[string]float64
	PlatformVersion  string
	Name             string
	ID               string
	Version          string
	Platform         string
	PlexPass         string
	Libraries        []library.Library
	HostCPU          float64
	HostMem          float64
	TransmitBytes    float64
	ActiveTranscodes int
	WSConnected      float64
	HTTPReachable    float64
}

// Snapshot returns a consistent point-in-time copy of the server's
// metric-visible state. Callers emit Prometheus metrics from the
// snapshot so Collect never holds s.Mu across a channel send.
func (s *Server) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		Name:             s.Name,
		ID:               s.ID,
		Version:          s.Version,
		Platform:         s.Platform,
		PlatformVersion:  s.PlatformVersion,
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
	if s.WSConnected {
		snap.WSConnected = 1.0
	}
	if s.HTTPReachable {
		snap.HTTPReachable = 1.0
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
				lb := &libs[i]
				for _, typ := range library.ItemCountTypes(lb.Type) {
					if count, ok := s.tryItemCount(ctx, lb.ID, typ); ok {
						lb.ItemsCount = count
						break
					}
				}
				if lb.ItemsCount == 0 {
					slog.Debug("library item count unavailable",
						"library", lb.Name, "id", lb.ID, "type", lb.Type)
				}
			}
		}()
	}
	wg.Wait()

	s.mu.Lock()
	for i, lb := range libs {
		if i < len(s.Libraries) && s.Libraries[i].ID == lb.ID {
			s.Libraries[i].ItemsCount = lb.ItemsCount
		}
	}
	s.mu.Unlock()
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
