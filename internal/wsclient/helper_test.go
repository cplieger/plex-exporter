package wsclient

import (
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/sessions"
)

// listenerFor builds a Listener wired for a test with noop error/connected hooks.
// Pass nil libs when the test doesn't exercise library resolution.
// Uses BackoffConfig injection instead of mutating package-level vars.
func listenerFor(client *plex.Client, tracker *sessions.Tracker, libs []library.Library) *Listener {
	return &Listener{
		Client:       client,
		Sessions:     tracker,
		Libraries:    func() []library.Library { return libs },
		RecordError:  func(string) {},
		SetConnected: func(bool) {},
		Backoff:      BackoffConfig{Min: time.Millisecond, Max: 10 * time.Millisecond},
	}
}
