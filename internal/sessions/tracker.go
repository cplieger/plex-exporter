package sessions

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plexapi"
)

// State is a normalised session playback state derived from the Plex
// /status/sessions Player.State string.
type State string

// State values observed from Plex session polling.
const (
	StatePlaying State = "playing"
	StateStopped State = "stopped"
	StatePaused  State = "paused"
	StateOther   State = "other"
)

// ParseState maps a raw Plex wire-protocol state string to a typed
// State constant. Unknown values map to StateOther so the state machine
// handles them intentionally rather than silently.
func ParseState(raw string) State {
	switch State(raw) {
	case StatePlaying:
		return StatePlaying
	case StateStopped:
		return StateStopped
	case StatePaused:
		return StatePaused
	default:
		return StateOther
	}
}

// defaultSessionTimeout bounds how long a stopped session may remain in the
// tracker map before prune reclaims it.
const defaultSessionTimeout = time.Minute

// defaultStaleSessionTimeout bounds how long a session can sit in a non-stopped
// state without receiving any update before we consider it orphaned and
// prune it.
const defaultStaleSessionTimeout = 5 * time.Minute

// MaxSessionKeyLen and MaxTrackedSessions bound the session tracker
// against a compromised or buggy Plex server that streams unbounded
// distinct sessionKey values. SessionKey is used both as a map key and
// as a Prometheus label value, so unbounded growth would OOM the
// exporter and inflate Mimir's active-series count.
const (
	MaxSessionKeyLen   = 64
	MaxTrackedSessions = 256
)

// Session is a single tracked Plex playback session. All fields are
// exported so callers (the Prometheus collector and the session poll
// handler in package server) can read and mutate the tracked state
// under the tracker's mutex without a wall of getter methods.
type Session struct {
	PlayStarted    time.Time
	LastUpdate     time.Time
	TranscodeType  string
	SubtitleAction string
	LibName        string
	LibID          string
	LibType        string
	State          State
	Meta           plexapi.SessionMetadata
	MediaMeta      plexapi.SessionMetadata
	PrevPlayedTime time.Duration
}

// PruneConfig holds prune timing parameters. Zero values mean "use
// package defaults".
type PruneConfig struct {
	SessionTimeout time.Duration
	StaleTimeout   time.Duration
	Interval       time.Duration
}

func (p PruneConfig) sessionTimeout() time.Duration {
	if p.SessionTimeout != 0 {
		return p.SessionTimeout
	}
	return defaultSessionTimeout
}

func (p PruneConfig) staleTimeout() time.Duration {
	if p.StaleTimeout != 0 {
		return p.StaleTimeout
	}
	return defaultStaleSessionTimeout
}

func (p PruneConfig) interval() time.Duration {
	if p.Interval != 0 {
		return p.Interval
	}
	return time.Minute
}

// Tracker is the in-memory active-session map. All lock-protected
// operations are encapsulated as methods; external callers must not
// access the mutex directly.
type Tracker struct {
	Sessions  map[string]Session
	PruneConf PruneConfig
	mu        sync.Mutex
}

// NewTracker returns an empty session tracker ready for use.
func NewTracker() *Tracker {
	return &Tracker{
		Sessions: make(map[string]Session),
	}
}

// normalizeKey truncates a session key to MaxSessionKeyLen so that
// write and lookup always use the same map key. It uses
// metrics.TruncateLabelValue (rune-aware truncation) deliberately: the
// normalized key is emitted verbatim as the `session` Prometheus label
// (collectSessions passes sessID through un-truncated), so it must stay
// valid UTF-8 — a plain byte-slice cut could split a multi-byte rune and
// make prometheus.MustNewConstMetric panic the collector goroutine.
func normalizeKey(id string) string {
	return metrics.TruncateLabelValue(id, MaxSessionKeyLen)
}

// UpdateLibraryLabels applies fn to the session identified by id under
// the tracker's lock. No-op if the session does not exist.
func (t *Tracker) UpdateLibraryLabels(id string, fn func(*Session)) {
	id = normalizeKey(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	if ss, ok := t.Sessions[id]; ok {
		fn(&ss)
		t.Sessions[id] = ss
	}
}

// SnapshotSessions returns a copy of the sessions map under the tracker's lock.
func (t *Tracker) SnapshotSessions() map[string]Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.Sessions)
}

// bankPlayTime banks a playing session's elapsed play time into
// PrevPlayedTime, keeping plex_play_seconds_total monotonic. Caller holds the tracker mutex.
func (s *Session) bankPlayTime(now time.Time) {
	elapsed := now.Sub(s.PlayStarted)
	s.PrevPlayedTime += elapsed
}

// Update records a state transition for the given session key. The meta
// and mediaMeta arguments may be nil; when non-nil they replace the
// cached metadata on the tracked session. On a playing→non-playing
// transition the elapsed play time is banked into PrevPlayedTime to keep
// plex_play_seconds_total monotonic.
func (t *Tracker) Update(id string, newState State, meta, mediaMeta *plexapi.SessionMetadata) {
	// Bound SessionKey length to prevent high-cardinality label explosion
	// from a malicious or buggy Plex server.
	id = normalizeKey(id)
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reject new sessions once the map is full; existing sessions continue
	// to update (so a legitimate session-churn during the cap doesn't drop
	// state). RunPruneLoop handles reclaiming stopped sessions.
	if _, existing := t.Sessions[id]; !existing && len(t.Sessions) >= MaxTrackedSessions {
		slog.Warn("session map full, dropping new session",
			"id", id, "tracked", len(t.Sessions), "cap", MaxTrackedSessions)
		return
	}

	s := t.Sessions[id]
	if meta != nil {
		s.Meta = *meta
	}
	if mediaMeta != nil {
		s.MediaMeta = *mediaMeta
	}

	// Bank play time on every playing→non-playing transition.
	if s.State == StatePlaying && newState != StatePlaying {
		s.bankPlayTime(time.Now())
	}
	// Reset play timer on non-playing→playing transition.
	if s.State != StatePlaying && newState == StatePlaying {
		s.PlayStarted = time.Now()
	}

	s.State = newState
	s.LastUpdate = time.Now()
	t.Sessions[id] = s
}

// MarkAbsentStopped transitions every tracked session whose key is not in
// presentKeys to StateStopped, so a stream that vanished from the poll
// moves onto the 60s stopped-prune path instead of lingering to the
// 5-minute stale timeout. A session already stopped is left untouched; a
// playing session banks its elapsed play time on the implicit
// playing->stopped transition.
func (t *Tracker) MarkAbsentStopped(presentKeys []string) {
	present := make(map[string]bool, len(presentKeys))
	for _, k := range presentKeys {
		present[normalizeKey(k)] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k := range t.Sessions {
		if present[k] {
			continue
		}
		s := t.Sessions[k]
		if s.State == StateStopped {
			continue
		}
		// Bank play time on the implicit playing→stopped transition.
		if s.State == StatePlaying {
			s.bankPlayTime(now)
		}
		s.State = StateStopped
		s.LastUpdate = now
		t.Sessions[k] = s
	}
}

// Prune reclaims stopped sessions past the session timeout and non-stopped
// sessions idle past the stale-session timeout. Safe to call concurrently
// with Update.
func (t *Tracker) Prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	sessionTO := t.PruneConf.sessionTimeout()
	staleTO := t.PruneConf.staleTimeout()
	var pruned, stale int
	for k := range t.Sessions {
		s := t.Sessions[k]
		remove := false
		switch {
		case s.State == StateStopped && time.Since(s.LastUpdate) > sessionTO:
			remove = true
			pruned++
		case s.State != StateStopped && time.Since(s.LastUpdate) > staleTO:
			slog.Warn("pruning stale non-stopped session",
				"id", k, "state", s.State, "idle", time.Since(s.LastUpdate))
			remove = true
			stale++
		}
		if remove {
			delete(t.Sessions, k)
		}
	}
	if pruned > 0 || stale > 0 {
		slog.Debug("pruned expired sessions",
			"stopped", pruned, "stale", stale, "remaining", len(t.Sessions))
	}
}

// RunPruneLoop invokes Prune on a configurable interval until ctx is cancelled.
func (t *Tracker) RunPruneLoop(ctx context.Context) {
	ticker := time.NewTicker(t.PruneConf.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.Prune()
		case <-ctx.Done():
			return
		}
	}
}
