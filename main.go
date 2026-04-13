package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// --- Constants ---

// healthFile is touched on startup and removed on shutdown.
// The "health" subcommand checks its existence for Docker healthchecks
// without requiring an HTTP server or open port.
const healthFile = "/tmp/.healthy"

// Response body size limit to prevent OOM on unexpected payloads.
const maxResponseBody = 10 << 20 // 10 MB

// retryBaseDelay is the base unit for exponential backoff in getWithRetry.
// Package-level var so tests can override for speed.
var retryBaseDelay = 100 * time.Millisecond

// Repeated string literals used across the codebase.
const (
	valUnknown   = "unknown"
	valNone      = "none"
	valFalse     = "false"
	valPending   = "pending"
	valTrue      = "true"
	valTranscode = "transcode"
	valBurn      = "burn"
	valCopy      = "copy"
	valBoth      = "both"
	valVideo     = "video"
	valAudio     = "audio"

	libMovie  = "movie"
	libShow   = "show"
	libArtist = "artist"
)

func main() {
	// CLI health probe for Docker healthcheck (distroless has no curl/wget).
	// Checks for a marker file instead of making an HTTP request — no port needed.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if _, err := os.Stat(healthFile); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	os.Exit(run())
}

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	serverAddr := requireEnv("PLEX_SERVER")
	plexToken := requireEnv("PLEX_TOKEN")
	listenAddr := envOr("LISTEN_ADDRESS", ":9594")

	// Remove stale health file from a previous run that may have crashed
	// before its defer ran. Without this, the health probe would report
	// healthy before the initial Plex connection succeeds.
	setHealthy(false)

	skipTLS := os.Getenv("SKIP_TLS_VERIFICATION")
	slog.Info("starting plex-exporter",
		"server", serverAddr, "listen", listenAddr,
		"skip_tls", skipTLS == "1" || skipTLS == "true")

	client := newPlexClient(serverAddr, plexToken)
	server := newPlexServer(client)

	if err := server.refresh(ctx); err != nil {
		slog.Error("cannot connect to plex server", "error", err)
		return 1
	}
	slog.Info("connected to plex server",
		"name", server.name, "version", server.version,
		"libraries", len(server.libraries))

	prometheus.MustRegister(server)
	go server.runRefreshLoop(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	go func() {
		slog.Info("starting metrics server", "addr", listenAddr)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server failed", "error", err)
			cancel()
		}
	}()

	setHealthy(true)
	defer setHealthy(false)

	server.listen(ctx)

	slog.Info("shutting down", "cause", context.Cause(ctx))
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown error", "error", err)
	}
	return 0
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error(key + " environment variable must be specified")
		os.Exit(1)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- Health ---

// setHealthy creates or removes the health marker file.
func setHealthy(ok bool) {
	if ok {
		if f, err := os.Create(healthFile); err == nil {
			f.Close()
		} else {
			slog.Warn("failed to create health file", "error", err)
		}
	} else {
		os.Remove(healthFile)
	}
}

// ---------------------------------------------------------------------------
// Plex HTTP client
// ---------------------------------------------------------------------------

type plexClient struct {
	httpClient *http.Client
	baseURL    *url.URL
	token      string
}

func newPlexClient(serverURL, token string) *plexClient {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		slog.Error("invalid PLEX_SERVER URL", "error", err)
		os.Exit(1)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if v := os.Getenv("SKIP_TLS_VERIFICATION"); v == "1" || v == valTrue {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, // G402: user-requested via env var
		}
	}
	return &plexClient{baseURL: parsed, token: token, httpClient: httpClient}
}

func (c *plexClient) get(ctx context.Context, path string, result any) error {
	return c.getWithHeaders(ctx, path, result, nil)
}

func (c *plexClient) getWithHeaders(ctx context.Context, path string, result any, extra map[string]string) error {
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL.ResolveReference(ref).String(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plex API %s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, result)
}

// getContainerSize fetches a library section with one item and reads the
// totalSize field from the JSON body. This is more reliable than the
// X-Plex-Container-Total-Size header, which doesn't work for type-filtered
// queries (e.g. ?type=4 for episodes).
func (c *plexClient) getContainerSize(ctx context.Context, path string) (int64, error) {
	var resp mc[struct {
		TotalSize int64 `json:"totalSize"`
	}]
	if err := c.getWithHeaders(ctx, path, &resp, map[string]string{
		"X-Plex-Container-Start": "0",
		"X-Plex-Container-Size":  "1",
	}); err != nil {
		return 0, err
	}
	return resp.MediaContainer.TotalSize, nil
}

func (c *plexClient) getWithRetry(ctx context.Context, path string, result any, maxRetries int) error {
	var lastErr error
	for attempt := range maxRetries {
		lastErr = c.get(ctx, path, result)
		if lastErr == nil {
			return nil
		}
		if attempt < maxRetries-1 {
			delay := time.NewTimer(retryBaseDelay * time.Duration(1<<attempt))
			select {
			case <-delay.C:
			case <-ctx.Done():
				delay.Stop()
				return ctx.Err()
			}
		}
	}
	return lastErr
}

var errNotFound = errors.New("not found")

// ---------------------------------------------------------------------------
// Plex API types
// ---------------------------------------------------------------------------

type mc[T any] struct {
	MediaContainer T `json:"MediaContainer"`
}

type serverIdentity struct {
	FriendlyName                  string `json:"friendlyName"`
	MachineIdentifier             string `json:"machineIdentifier"`
	Version                       string `json:"version"`
	Platform                      string `json:"platform"`
	PlatformVersion               string `json:"platformVersion"`
	MyPlexSubscription            bool   `json:"myPlexSubscription"`
	TranscoderActiveVideoSessions int    `json:"transcoderActiveVideoSessions"`
}

type mediaProviderResponse struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
	MediaProviders    []struct {
		Identifier string `json:"identifier"`
		Features   []struct {
			Type        string `json:"type"`
			Directories []struct {
				Title         string `json:"title"`
				ID            string `json:"id"`
				Type          string `json:"type"`
				DurationTotal int64  `json:"durationTotal"`
				StorageTotal  int64  `json:"storageTotal"`
			} `json:"Directory"`
		} `json:"Feature"`
	} `json:"MediaProvider"`
}

type statisticsResource struct {
	HostCPUUtil float64 `json:"hostCpuUtilization"`
	HostMemUtil float64 `json:"hostMemoryUtilization"`
}

type statisticsBandwidth struct {
	Bytes int64 `json:"bytes"`
	At    int   `json:"at"`
}

type mediaInfo struct {
	VideoResolution string `json:"videoResolution"`
	VideoCodec      string `json:"videoCodec"`
	AudioCodec      string `json:"audioCodec"`
	Part            []struct {
		Decision string `json:"decision"`
		Key      string `json:"key"`
	} `json:"Part"`
	Bitrate int `json:"bitrate"`
}

type sessionMetadata struct {
	Player struct {
		Device  string `json:"device"`
		Product string `json:"product"`
		State   string `json:"state"`
		Local   bool   `json:"local"`
		Relayed bool   `json:"relayed"`
		Secure  bool   `json:"secure"`
	} `json:"Player"`
	Session struct {
		Location  string `json:"location"`
		Bandwidth int    `json:"bandwidth"`
	} `json:"Session"`
	User struct {
		Title string `json:"title"`
		ID    string `json:"id"`
	} `json:"User"`
	GrandparentTitle string      `json:"grandparentTitle"`
	ParentTitle      string      `json:"parentTitle"`
	Title            string      `json:"title"`
	Type             string      `json:"type"`
	LibrarySectionID json.Number `json:"librarySectionID"`
	SessionKey       string      `json:"sessionKey"`
	RatingKey        string      `json:"ratingKey"`
	Media            []mediaInfo `json:"Media"`
}

// metadataListResponse is shared by /status/sessions and /library/metadata/<id>.
type metadataListResponse struct {
	Metadata []sessionMetadata `json:"Metadata"`
}

type wsTranscodeSession struct {
	Key              string `json:"key"`
	VideoDecision    string `json:"videoDecision"`
	AudioDecision    string `json:"audioDecision"`
	SubtitleDecision string `json:"subtitleDecision"`
	SourceVideoCodec string `json:"sourceVideoCodec"`
	SourceAudioCodec string `json:"sourceAudioCodec"`
	VideoCodec       string `json:"videoCodec"`
	AudioCodec       string `json:"audioCodec"`
	Container        string `json:"container"`
}

type wsPlayNotification struct {
	SessionKey       string `json:"sessionKey"`
	RatingKey        string `json:"ratingKey"`
	State            string `json:"state"`
	TranscodeSession string `json:"transcodeSession"`
	ViewOffset       int64  `json:"viewOffset"`
}

type wsNotification struct {
	NotificationContainer struct {
		Type                         string               `json:"type"`
		PlaySessionStateNotification []wsPlayNotification `json:"PlaySessionStateNotification"`
		TranscodeSession             []wsTranscodeSession `json:"TranscodeSession"`
	} `json:"NotificationContainer"`
}

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

var (
	srvLabels  = []string{"server", "server_id"}
	libLabels  = []string{"server", "server_id", "library_type", "library", "library_id"}
	playLabels = []string{
		"server", "server_id",
		"library", "library_id", "library_type",
		"media_type", "title", "child_title", "grandchild_title",
		"stream_type", "stream_resolution", "stream_file_resolution",
		"stream_bitrate", "device", "device_type", "user", "session",
		"transcode_type", "subtitle_action", "location", "local",
	}

	descServerInfo = prometheus.NewDesc(
		"plex_server_info", "Plex server information",
		append(srvLabels, "version", "platform", "platform_version", "plex_pass"), nil)
	descHostCPU = prometheus.NewDesc(
		"plex_host_cpu_utilization_ratio", "Host CPU utilization (0-1)",
		srvLabels, nil)
	descHostMem = prometheus.NewDesc(
		"plex_host_memory_utilization_ratio", "Host memory utilization (0-1)",
		srvLabels, nil)
	descLibDuration = prometheus.NewDesc(
		"plex_library_duration_milliseconds", "Total library duration in ms",
		libLabels, nil)
	descLibStorage = prometheus.NewDesc(
		"plex_library_storage_bytes", "Total library storage in bytes",
		libLabels, nil)
	descLibItems = prometheus.NewDesc(
		"plex_library_items", "Number of items in a library section",
		append(libLabels, "content_type"), nil)
	descTransmitBytes = prometheus.NewDesc(
		"plex_transmit_bytes_total", "Bytes transmitted (bandwidth API)",
		srvLabels, nil)
	descActiveTranscodes = prometheus.NewDesc(
		"plex_active_transcode_sessions", "Active transcode sessions",
		srvLabels, nil)
	descPlayCount = prometheus.NewDesc(
		"plex_plays_total", "Active play sessions (1 per session)",
		playLabels, nil)
	descPlaySeconds = prometheus.NewDesc(
		"plex_play_seconds_total", "Total play time per session",
		playLabels, nil)
	descSessionBandwidth = prometheus.NewDesc(
		"plex_session_bandwidth_kbps", "Session bandwidth in kbps",
		append(srvLabels, "session", "user", "location"), nil)
	descEstTransmitBytes = prometheus.NewDesc(
		"plex_estimated_transmit_bytes_total", "Estimated bytes from bitrates",
		srvLabels, nil)
	descWSConnected = prometheus.NewDesc(
		"plex_websocket_connected", "Websocket connection status (1/0)",
		srvLabels, nil)
)

// ---------------------------------------------------------------------------
// Library
// ---------------------------------------------------------------------------

type library struct {
	ID, Name, Type string
	DurationTotal  int64
	StorageTotal   int64
	ItemsCount     int64
}

func isLibraryType(t string) bool {
	switch t {
	case libMovie, libShow, libArtist, "photo", "homevideo":
		return true
	}
	return false
}

func contentTypeLabel(libType string) string {
	switch libType {
	case libMovie:
		return "movies"
	case libShow:
		return "episodes"
	case libArtist:
		return "tracks"
	case "photo":
		return "photos"
	default:
		return "items"
	}
}

// buildLibraries extracts library entries from the media providers response,
// preserving existing item counts from prevItems.
func buildLibraries(providers mediaProviderResponse, prevItems map[string]int64) []library {
	var libs []library
	for _, p := range providers.MediaProviders {
		if p.Identifier != "com.plexapp.plugins.library" {
			continue
		}
		for _, f := range p.Features {
			if f.Type != "content" {
				continue
			}
			for _, d := range f.Directories {
				if !isLibraryType(d.Type) {
					continue
				}
				libs = append(libs, library{
					ID: d.ID, Name: d.Title, Type: d.Type,
					DurationTotal: d.DurationTotal, StorageTotal: d.StorageTotal,
					ItemsCount: prevItems[d.ID],
				})
			}
		}
	}
	return libs
}

// ---------------------------------------------------------------------------
// Session tracking
// ---------------------------------------------------------------------------

type sessionState string

const (
	statePlaying sessionState = "playing"
	stateStopped sessionState = "stopped"
)

const sessionTimeout = time.Minute

type session struct {
	playStarted    time.Time
	lastUpdate     time.Time
	transcodeType  string
	subtitleAction string
	libName        string
	libID          string
	libType        string
	state          sessionState
	meta           sessionMetadata
	mediaMeta      sessionMetadata
	prevPlayedTime time.Duration
}

type sessionTracker struct {
	sessions            map[string]session
	mu                  sync.Mutex
	totalEstimatedKBits float64
}

func newSessionTracker() *sessionTracker {
	return &sessionTracker{sessions: make(map[string]session)}
}

func (t *sessionTracker) update(id string, newState sessionState, meta, mediaMeta *sessionMetadata) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.sessions[id]
	if meta != nil {
		s.meta = *meta
	}
	if mediaMeta != nil {
		s.mediaMeta = *mediaMeta
	}

	// Accumulate play time and estimated bandwidth on playing→non-playing transition.
	if s.state == statePlaying && newState != statePlaying && len(s.meta.Media) > 0 {
		elapsed := time.Since(s.playStarted)
		s.prevPlayedTime += elapsed
		t.totalEstimatedKBits += elapsed.Seconds() * float64(s.meta.Media[0].Bitrate)
	}
	// Reset play timer on non-playing→playing transition.
	if s.state != statePlaying && newState == statePlaying {
		s.playStarted = time.Now()
	}

	s.state = newState
	s.lastUpdate = time.Now()
	t.sessions[id] = s
}

func (t *sessionTracker) prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	var pruned int
	for k := range t.sessions {
		s := t.sessions[k]
		if s.state == stateStopped && time.Since(s.lastUpdate) > sessionTimeout {
			delete(t.sessions, k)
			pruned++
		}
	}
	if pruned > 0 {
		slog.Debug("pruned expired sessions", "count", pruned, "remaining", len(t.sessions))
	}
}

func (t *sessionTracker) runPruneLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.prune()
		case <-ctx.Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Plex server
// ---------------------------------------------------------------------------

type plexServer struct {
	// Refresh state
	lastItemsRefresh time.Time

	// Dependencies
	client   *plexClient
	sessions *sessionTracker

	// Server identity
	id              string
	name            string
	version         string
	platform        string
	platformVersion string

	// Metrics state
	libraries        []library
	hostMem          float64
	transmitBytes    float64
	hostCPU          float64
	lastBandwidthAt  int
	activeTranscodes int

	// Concurrency
	mu sync.Mutex

	// Flags
	plexPass    bool
	wsConnected bool
}

func newPlexServer(client *plexClient) *plexServer {
	return &plexServer{
		client:          client,
		lastBandwidthAt: int(time.Now().Unix()),
		sessions:        newSessionTracker(),
	}
}

func (s *plexServer) refresh(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	// Server identity + library list.
	var providers mc[mediaProviderResponse]
	if err := s.client.get(ctx, "/media/providers?includeStorage=1", &providers); err != nil {
		return fmt.Errorf("fetching providers: %w", err)
	}

	s.mu.Lock()
	s.id = providers.MediaContainer.MachineIdentifier
	s.name = providers.MediaContainer.FriendlyName
	s.version = providers.MediaContainer.Version

	// Build a lookup of existing item counts so they survive the rebuild.
	prevItems := make(map[string]int64, len(s.libraries))
	for _, lib := range s.libraries {
		if lib.ItemsCount > 0 {
			prevItems[lib.ID] = lib.ItemsCount
		}
	}

	s.libraries = buildLibraries(providers.MediaContainer, prevItems)
	needItemsRefresh := time.Since(s.lastItemsRefresh) > 15*time.Minute
	s.mu.Unlock()

	// Server info from root endpoint.
	var info mc[serverIdentity]
	if err := s.client.get(ctx, "/", &info); err != nil {
		return fmt.Errorf("fetching server info: %w", err)
	}
	s.mu.Lock()
	s.platform = info.MediaContainer.Platform
	s.platformVersion = info.MediaContainer.PlatformVersion
	s.plexPass = info.MediaContainer.MyPlexSubscription
	s.activeTranscodes = info.MediaContainer.TranscoderActiveVideoSessions
	s.mu.Unlock()

	// Library item counts (every 15 minutes).
	if needItemsRefresh {
		s.refreshLibraryItems(ctx)
		s.mu.Lock()
		s.lastItemsRefresh = time.Now()
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

func (s *plexServer) refreshLibraryItems(ctx context.Context) {
	s.mu.Lock()
	libs := make([]library, len(s.libraries))
	copy(libs, s.libraries)
	s.mu.Unlock()

	for i := range libs {
		lib := &libs[i]
		path := "/library/sections/" + lib.ID + "/all"
		// For show libraries, count episodes (type=4) instead of shows.
		if lib.Type == libShow {
			path += "?type=4"
		}
		// For music libraries, count tracks (type=10, fallback to 7).
		if lib.Type == libArtist {
			count, err := s.client.getContainerSize(ctx, "/library/sections/"+lib.ID+"/all?type=10")
			if err == nil && count > 0 {
				lib.ItemsCount = count
				continue
			}
			count, err = s.client.getContainerSize(ctx, "/library/sections/"+lib.ID+"/all?type=7")
			if err == nil && count > 0 {
				lib.ItemsCount = count
				continue
			}
		}
		count, err := s.client.getContainerSize(ctx, path)
		if err == nil {
			lib.ItemsCount = count
		} else {
			slog.Debug("library item count unavailable",
				"library", lib.Name, "id", lib.ID, "error", err)
		}
	}

	s.mu.Lock()
	for i, lib := range libs {
		if i < len(s.libraries) && s.libraries[i].ID == lib.ID {
			s.libraries[i].ItemsCount = lib.ItemsCount
		}
	}
	s.mu.Unlock()
}

func (s *plexServer) refreshResources(ctx context.Context) {
	var resp mc[struct {
		StatisticsResources []statisticsResource `json:"StatisticsResources"`
	}]
	if err := s.client.get(ctx, "/statistics/resources?timespan=6", &resp); err != nil {
		slog.Debug("resources unavailable", "error", err)
		return
	}
	stats := resp.MediaContainer.StatisticsResources
	if len(stats) == 0 {
		return
	}
	latest := stats[len(stats)-1]
	s.mu.Lock()
	s.hostCPU = latest.HostCPUUtil / 100
	s.hostMem = latest.HostMemUtil / 100
	s.mu.Unlock()
}

func (s *plexServer) refreshBandwidth(ctx context.Context) {
	var resp mc[struct {
		StatisticsBandwidth []statisticsBandwidth `json:"StatisticsBandwidth"`
	}]
	if err := s.client.get(ctx, "/statistics/bandwidth?timespan=6", &resp); err != nil {
		slog.Debug("bandwidth unavailable", "error", err)
		return
	}
	updates := resp.MediaContainer.StatisticsBandwidth
	slices.SortFunc(updates, func(a, b statisticsBandwidth) int { return a.At - b.At })

	s.mu.Lock()
	defer s.mu.Unlock()
	highest := s.lastBandwidthAt
	for _, u := range updates {
		if u.At > s.lastBandwidthAt {
			s.transmitBytes += float64(u.Bytes)
			if u.At > highest {
				highest = u.At
			}
		}
	}
	s.lastBandwidthAt = highest
}

func (s *plexServer) runRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := s.refresh(ctx); err != nil {
				slog.Warn("refresh failed", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Prometheus Collector
// ---------------------------------------------------------------------------

func (s *plexServer) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descServerInfo, descHostCPU, descHostMem,
		descLibDuration, descLibStorage, descLibItems,
		descTransmitBytes, descActiveTranscodes,
		descPlayCount, descPlaySeconds, descSessionBandwidth,
		descEstTransmitBytes, descWSConnected,
	} {
		ch <- d
	}
}

func (s *plexServer) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	name, id := s.name, s.id
	version := s.version
	platform := s.platform
	platformVersion := s.platformVersion
	plexPass := valFalse
	if s.plexPass {
		plexPass = valTrue
	}
	hostCPU := s.hostCPU
	hostMem := s.hostMem
	transmitBytes := s.transmitBytes
	activeTranscodes := s.activeTranscodes
	wsVal := 0.0
	if s.wsConnected {
		wsVal = 1.0
	}
	libs := make([]library, len(s.libraries))
	copy(libs, s.libraries)
	s.mu.Unlock()

	ch <- prometheus.MustNewConstMetric(descServerInfo, prometheus.GaugeValue, 1,
		name, id, version, platform, platformVersion, plexPass)
	ch <- prometheus.MustNewConstMetric(descHostCPU, prometheus.GaugeValue, hostCPU, name, id)
	ch <- prometheus.MustNewConstMetric(descHostMem, prometheus.GaugeValue, hostMem, name, id)
	ch <- prometheus.MustNewConstMetric(descTransmitBytes, prometheus.CounterValue, transmitBytes, name, id)
	ch <- prometheus.MustNewConstMetric(descActiveTranscodes, prometheus.GaugeValue,
		float64(activeTranscodes), name, id)
	ch <- prometheus.MustNewConstMetric(descWSConnected, prometheus.GaugeValue, wsVal, name, id)

	for _, lib := range libs {
		ch <- prometheus.MustNewConstMetric(descLibDuration, prometheus.GaugeValue,
			float64(lib.DurationTotal), name, id, lib.Type, lib.Name, lib.ID)
		ch <- prometheus.MustNewConstMetric(descLibStorage, prometheus.GaugeValue,
			float64(lib.StorageTotal), name, id, lib.Type, lib.Name, lib.ID)
		if lib.ItemsCount > 0 {
			ch <- prometheus.MustNewConstMetric(descLibItems, prometheus.GaugeValue,
				float64(lib.ItemsCount), name, id, lib.Type, lib.Name, lib.ID,
				contentTypeLabel(lib.Type))
		}
	}
	s.collectSessions(ch, name, id, libs)
}

func (s *plexServer) collectSessions(ch chan<- prometheus.Metric, srvName, srvID string, libs []library) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()

	libByID := make(map[string]library, len(libs))
	for _, l := range libs {
		libByID[l.ID] = l
	}

	estTotal := s.sessions.totalEstimatedKBits
	for sessID := range s.sessions.sessions {
		sess := s.sessions.sessions[sessID]

		// Accumulate estimated transmit for playing sessions.
		if sess.state == statePlaying && len(sess.meta.Media) > 0 {
			estTotal += time.Since(sess.playStarted).Seconds() * float64(sess.meta.Media[0].Bitrate)
		}

		if sess.playStarted.IsZero() {
			continue
		}

		labelVals := sessionLabelValues(srvName, srvID, &sess, sessID, libByID)

		ch <- prometheus.MustNewConstMetric(descPlayCount, prometheus.CounterValue, 1, labelVals...)

		totalPlay := sess.prevPlayedTime
		if sess.state == statePlaying {
			totalPlay += time.Since(sess.playStarted)
		}
		ch <- prometheus.MustNewConstMetric(descPlaySeconds, prometheus.CounterValue,
			totalPlay.Seconds(), labelVals...)

		// Per-session bandwidth from the sessions API.
		if sess.meta.Session.Bandwidth > 0 {
			ch <- prometheus.MustNewConstMetric(descSessionBandwidth, prometheus.GaugeValue,
				float64(sess.meta.Session.Bandwidth),
				srvName, srvID, sessID, sess.meta.User.Title,
				orDefault(sess.meta.Session.Location, valUnknown))
		}
	}

	// Estimated transmit bytes.
	ch <- prometheus.MustNewConstMetric(descEstTransmitBytes, prometheus.CounterValue,
		estTotal*128, srvName, srvID)
}

// maxLabelLen is the maximum byte length for user-controlled Prometheus label values.
const maxLabelLen = 128

// truncLabel truncates a label value to maxLabelLen bytes to prevent
// high-cardinality label sets from user-controlled Plex API data.
// It respects UTF-8 boundaries to avoid splitting multi-byte codepoints.
func truncLabel(s string) string {
	if len(s) <= maxLabelLen {
		return s
	}
	// Walk backward from the byte limit to find a valid UTF-8 boundary.
	i := maxLabelLen
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// sessionLabelValues builds the Prometheus label value slice for a session.
func sessionLabelValues(
	srvName, srvID string,
	sess *session, sessID string,
	libByID map[string]library,
) []string {
	streamType, streamRes, bitrate := valUnknown, "", "0"
	if len(sess.meta.Media) > 0 {
		m := sess.meta.Media[0]
		if len(m.Part) > 0 && m.Part[0].Decision != "" {
			streamType = m.Part[0].Decision
		}
		streamRes = m.VideoResolution
		if m.Bitrate != 0 {
			bitrate = strconv.Itoa(m.Bitrate)
		}
	}
	fileRes := ""
	if len(sess.mediaMeta.Media) > 0 {
		fileRes = sess.mediaMeta.Media[0].VideoResolution
	}

	libName, libID, libType := sess.libName, sess.libID, sess.libType
	if libName == "" {
		if lib, ok := libByID[sess.mediaMeta.LibrarySectionID.String()]; ok {
			libName, libID, libType = lib.Name, lib.ID, lib.Type
		} else {
			libName, libID, libType = valUnknown, "0", valUnknown
		}
	}

	title, childTitle, grandchildTitle := sessionLabels(&sess.mediaMeta)
	title = truncLabel(title)
	childTitle = truncLabel(childTitle)
	grandchildTitle = truncLabel(grandchildTitle)

	ttype := orDefault(sess.transcodeType, valNone)
	if ttype == valPending {
		ttype = valNone
	}
	subtitle := orDefault(sess.subtitleAction, valNone)
	location := orDefault(sess.meta.Session.Location, valUnknown)
	local := valFalse
	if sess.meta.Player.Local {
		local = valTrue
	}

	return []string{
		srvName, srvID, libName, libID, libType,
		sess.mediaMeta.Type, title, childTitle, grandchildTitle,
		streamType, streamRes, fileRes, bitrate,
		sess.meta.Player.Device, sess.meta.Player.Product,
		truncLabel(sess.meta.User.Title), sessID,
		ttype, subtitle, location, local,
	}
}

func sessionLabels(m *sessionMetadata) (title, child, grandchild string) {
	// Episodes: Show / Season / Episode. Tracks: Artist / Album / Track.
	switch m.Type {
	case "episode", "track":
		return m.GrandparentTitle, m.ParentTitle, m.Title
	default:
		return m.Title, "", ""
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------------------
// Websocket listener with reconnection
// ---------------------------------------------------------------------------

func (s *plexServer) listen(ctx context.Context) {
	go s.sessions.runPruneLoop(ctx)

	const (
		minBackoff = time.Second
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := s.connectAndListen(ctx)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.wsConnected = false
		s.mu.Unlock()

		// Reset backoff after a successful connection — the disconnect is
		// likely transient. Only escalate backoff for repeated dial failures.
		if connected {
			backoff = minBackoff
		}

		slog.Warn("websocket disconnected, reconnecting", "error", err, "backoff", backoff)
		delay := time.NewTimer(backoff)
		select {
		case <-delay.C:
		case <-ctx.Done():
			delay.Stop()
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (s *plexServer) connectAndListen(ctx context.Context) (bool, error) {
	scheme := "ws"
	if s.client.baseURL.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := url.URL{
		Scheme: scheme,
		Host:   s.client.baseURL.Host,
		Path:   "/:/websockets/notifications",
	}

	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Plex-Token": {s.client.token}},
		HTTPClient: s.client.httpClient,
	}

	conn, resp, err := websocket.Dial(ctx, wsURL.String(), opts)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("websocket dial: %w", err)
	}
	defer func() {
		if closeErr := conn.CloseNow(); closeErr != nil {
			slog.Debug("websocket close", "error", closeErr)
		}
	}()

	s.mu.Lock()
	s.wsConnected = true
	serverName, serverID := s.name, s.id
	s.mu.Unlock()
	slog.Info("websocket connected", "server", serverName, "id", serverID)

	// Limit WebSocket message size to prevent OOM from oversized messages.
	conn.SetReadLimit(1 << 20) // 1 MB

	for {
		_, message, readErr := conn.Read(ctx)
		if readErr != nil {
			return true, fmt.Errorf("websocket read: %w", readErr)
		}
		var notif wsNotification
		if jsonErr := json.Unmarshal(message, &notif); jsonErr != nil {
			slog.Warn("invalid websocket message", "error", jsonErr)
			continue
		}
		switch notif.NotificationContainer.Type {
		case "playing":
			s.handlePlaying(ctx, notif)
		case "transcodeSession.update":
			s.handleTranscodeUpdate(notif)
		}
	}
}

func (s *plexServer) handlePlaying(ctx context.Context, notif wsNotification) {
	var sessResp mc[metadataListResponse]
	if err := s.client.getWithRetry(ctx, "/status/sessions", &sessResp, 3); err != nil {
		slog.Warn("failed to fetch sessions", "error", err)
		return
	}

	sessionsByKey := make(map[string]*sessionMetadata, len(sessResp.MediaContainer.Metadata))
	for i := range sessResp.MediaContainer.Metadata {
		m := &sessResp.MediaContainer.Metadata[i]
		sessionsByKey[m.SessionKey] = m
	}

	for _, n := range notif.NotificationContainer.PlaySessionStateNotification {
		state := sessionState(n.State)
		if state == stateStopped {
			s.sessions.update(n.SessionKey, state, nil, nil)
			continue
		}

		sess, ok := sessionsByKey[n.SessionKey]
		if !ok {
			slog.Debug("session not found", "key", n.SessionKey)
			continue
		}

		var metaResp mc[metadataListResponse]
		if _, err := strconv.Atoi(n.RatingKey); err != nil {
			slog.Warn("invalid rating key", "key", n.RatingKey)
			continue
		}
		if err := s.client.getWithRetry(ctx, "/library/metadata/"+n.RatingKey, &metaResp, 3); err != nil {
			slog.Warn("failed to fetch metadata", "key", n.RatingKey, "error", err)
			continue
		}
		if len(metaResp.MediaContainer.Metadata) == 0 {
			slog.Debug("empty metadata response", "key", n.RatingKey)
			continue
		}
		media := &metaResp.MediaContainer.Metadata[0]

		slog.Info("play event",
			"session", n.SessionKey, "user", sess.User.Title,
			"state", n.State, "title", media.Title,
			"offset", time.Duration(n.ViewOffset)*time.Millisecond)

		s.sessions.update(n.SessionKey, state, sess, media)

		// Persist resolved library labels.
		// Copy libraries under s.mu first, then update session under s.sessions.mu.
		// This avoids holding both locks simultaneously (s.mu → s.sessions.mu is the
		// canonical order used by Collect → collectSessions; inverting it deadlocks).
		s.mu.Lock()
		libs := make([]library, len(s.libraries))
		copy(libs, s.libraries)
		s.mu.Unlock()

		s.sessions.mu.Lock()
		if ss, ok := s.sessions.sessions[n.SessionKey]; ok {
			resolveSessionLibrary(&ss, media, libs, n.TranscodeSession)
			s.sessions.sessions[n.SessionKey] = ss
		}
		s.sessions.mu.Unlock()
	}
}

// resolveSessionLibrary fills in library labels from the library list
// if the session doesn't already have them, and marks transcode pending
// if the notification includes a transcode session key.
func resolveSessionLibrary(ss *session, media *sessionMetadata, libs []library, transcodeSession string) {
	if ss.libName == "" {
		for _, lib := range libs {
			if lib.ID != media.LibrarySectionID.String() {
				continue
			}
			ss.libName = lib.Name
			ss.libID = lib.ID
			ss.libType = lib.Type
			break
		}
	}
	if transcodeSession != "" && ss.transcodeType == "" {
		ss.transcodeType = valPending
	}
}

func (s *plexServer) handleTranscodeUpdate(notif wsNotification) {
	for i := range notif.NotificationContainer.TranscodeSession {
		ts := &notif.NotificationContainer.TranscodeSession[i]
		kind := transcodeKind(ts)
		subtitle := subtitleAction(ts)

		slog.Debug("transcode update", "key", ts.Key, "type", kind, "subtitle", subtitle)

		s.sessions.mu.Lock()
		matched := false
		for k := range s.sessions.sessions {
			ss := s.sessions.sessions[k]
			if !matchesTranscode(&ss, ts.Key) {
				continue
			}
			ss.transcodeType = kind
			ss.subtitleAction = subtitle
			s.sessions.sessions[k] = ss
			matched = true
			break
		}
		s.sessions.mu.Unlock()

		if !matched {
			slog.Debug("transcode update unmatched", "key", ts.Key, "kind", kind)
		}
	}
}

// ---------------------------------------------------------------------------
// Transcode and subtitle classification
// ---------------------------------------------------------------------------

func matchesTranscode(ss *session, transcodeKey string) bool {
	if ss.transcodeType == valPending {
		return true
	}
	for _, m := range ss.meta.Media {
		for _, p := range m.Part {
			if p.Key != "" && strings.Contains(p.Key, transcodeKey) {
				return true
			}
		}
	}
	return false
}

func transcodeKind(ts *wsTranscodeSession) string {
	vDec := strings.ToLower(strings.TrimSpace(ts.VideoDecision))
	aDec := strings.ToLower(strings.TrimSpace(ts.AudioDecision))
	vSrc := strings.ToLower(strings.TrimSpace(ts.SourceVideoCodec))
	vNew := strings.ToLower(strings.TrimSpace(ts.VideoCodec))
	aSrc := strings.ToLower(strings.TrimSpace(ts.SourceAudioCodec))
	aNew := strings.ToLower(strings.TrimSpace(ts.AudioCodec))

	hasVideo := vDec == valTranscode || (vNew != "" && vNew != vSrc)
	hasAudio := aDec == valTranscode || (aNew != "" && aNew != aSrc)

	switch {
	case hasVideo && hasAudio:
		return valBoth
	case hasVideo:
		return valVideo
	case hasAudio:
		return valAudio
	default:
		return valNone
	}
}

func subtitleAction(ts *wsTranscodeSession) string {
	sd := strings.ToLower(strings.TrimSpace(ts.SubtitleDecision))
	switch sd {
	case valBurn, "burn-in":
		return valBurn
	case valCopy, "copying":
		return valCopy
	case valTranscode, "transcoding":
		return valTranscode
	case "":
		ctn := strings.ToLower(strings.TrimSpace(ts.Container))
		if ctn == "srt" || strings.Contains(ctn, "srt") {
			return valCopy
		}
		if strings.ToLower(strings.TrimSpace(ts.VideoDecision)) == valTranscode {
			return valBurn
		}
		return valNone
	default:
		return sd
	}
}
