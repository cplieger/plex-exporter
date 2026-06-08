package wsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/plexapi"
	"github.com/cplieger/plex-exporter/internal/sessions"
)

// PingInterval is how often we send a Ping while the WebSocket is
// connected. A failed ping closes the connection so the main read loop
// returns, triggering a normal reconnect. This catches half-open
// connections (NAT/middlebox state dropped without RST).
const PingInterval = 30 * time.Second

// PongTimeout is the deadline for a single ping round-trip.
const PongTimeout = 10 * time.Second

// Plex WebSocket notification type identifiers. These match the
// `NotificationContainer.type` field in Plex's /:/websockets/notifications
// protocol and gate which handler runs for each message.
const (
	NotifTypePlaying         = "playing"
	NotifTypeTranscodeUpdate = "transcodeSession.update"
)

// BackoffConfig holds reconnect backoff parameters. Zero values mean
// "use package defaults" (1s min, 30s max).
type BackoffConfig struct {
	Min time.Duration
	Max time.Duration
}

// Listener opens and maintains the Plex websocket notification channel,
// dispatching events into the session tracker. Dependencies are injected
// as closures and a narrow interface so the listener stays free of any
// import-cycle back-edge to package server; the composition root in
// package main wires the callbacks.
type Listener struct {
	Client       *plex.Client
	Sessions     *sessions.Tracker
	Libraries    func() []library.Library
	RecordError  func(kind string)
	SetConnected func(bool)
	Backoff      BackoffConfig
}

// Listen runs the reconnect loop until ctx is cancelled. It maintains
// the canonical backoff / sawTraffic invariant: only a connection that
// carried traffic resets backoff; a connection that never carried a
// byte records a websocket_dial error and escalates.
func (l *Listener) Listen(ctx context.Context) {
	// SetConnected(false) on return so external observers (Prometheus
	// scrape, integration tests) see an unambiguous "not connected"
	// state after Listen exits.
	defer l.SetConnected(false)

	backoff := l.backoffMin()

	for {
		if ctx.Err() != nil {
			return
		}
		sawTraffic, err := l.ConnectAndListen(ctx)
		l.SetConnected(false)
		if ctx.Err() != nil {
			return
		}

		// Only reset backoff once the WebSocket actually carried traffic.
		// A server that accepts the handshake and immediately drops the
		// socket (Plex restart, auth invalidated, LB misbehaviour) would
		// otherwise keep us in a 1s tight-reconnect loop forever.
		if sawTraffic {
			backoff = l.backoffMin()
		} else {
			l.RecordError("websocket_dial")
		}

		slog.Warn("websocket disconnected, reconnecting", "error", err, "backoff", backoff)
		delay := time.NewTimer(backoff)
		select {
		case <-delay.C:
		case <-ctx.Done():
			delay.Stop()
			return
		}
		backoff = min(backoff*2, l.backoffMax())
	}
}

// ConnectAndListen opens the WebSocket and blocks in the read loop
// until the connection ends. Returns (sawTraffic, err): sawTraffic is
// true once at least one message has been received; this is the signal
// the caller uses to distinguish "healthy connection dropped" from
// "accept-and-reject" loops.
func (l *Listener) ConnectAndListen(ctx context.Context) (bool, error) {
	conn, err := l.dialWebsocket(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := conn.CloseNow(); closeErr != nil {
			slog.Debug("websocket close", "error", closeErr)
		}
	}()

	l.SetConnected(true)
	slog.Info("websocket connected")

	// Limit WebSocket message size to prevent OOM from oversized messages.
	conn.SetReadLimit(1 << 20) // 1 MB

	// Keep-alive ping goroutine: detects half-open connections that
	// middleboxes drop without RST. A failed ping closes the connection
	// so the Read loop below returns with an error.
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go l.pingLoop(pingCtx, conn)

	return l.readNotifications(ctx, conn)
}

func (l *Listener) backoffMin() time.Duration {
	if l.Backoff.Min != 0 {
		return l.Backoff.Min
	}
	return time.Second
}

func (l *Listener) backoffMax() time.Duration {
	if l.Backoff.Max != 0 {
		return l.Backoff.Max
	}
	return 30 * time.Second
}

// dialWebsocket opens the WebSocket to /:/websockets/notifications.
func (l *Listener) dialWebsocket(ctx context.Context) (*websocket.Conn, error) {
	scheme := "ws"
	if l.Client.BaseURL.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := url.URL{
		Scheme: scheme,
		Host:   l.Client.BaseURL.Host,
		Path:   "/:/websockets/notifications",
	}
	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Plex-Token": {l.Client.Token}},
		HTTPClient: l.Client.HTTPClient,
	}
	conn, resp, err := websocket.Dial(ctx, wsURL.String(), opts)
	// coder/websocket Dial may return a non-nil resp with a nil Body when
	// the handshake fails partway. Guard both to avoid a nil-deref panic.
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	return conn, nil
}

// pingLoop periodically sends a ping and closes the connection on failure.
func (l *Listener) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, PongTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				slog.Warn("websocket ping failed, closing", "error", err)
				if closeErr := conn.CloseNow(); closeErr != nil {
					slog.Debug("websocket close after failed ping", "error", closeErr)
				}
				return
			}
		}
	}
}

// readNotifications blocks reading websocket messages until the
// connection ends or the context is cancelled. Returns (sawTraffic,
// err) where sawTraffic is true once any message has been read.
func (l *Listener) readNotifications(ctx context.Context, conn *websocket.Conn) (bool, error) {
	var sawTraffic bool
	for {
		_, message, readErr := conn.Read(ctx)
		if readErr != nil {
			if sawTraffic {
				l.RecordError("websocket_read")
			}
			return sawTraffic, fmt.Errorf("websocket read: %w", readErr)
		}
		sawTraffic = true
		var notif plexapi.WSNotification
		if jsonErr := json.Unmarshal(message, &notif); jsonErr != nil {
			slog.Warn("invalid websocket message", "error", jsonErr)
			l.RecordError("invalid_message")
			continue
		}
		switch notif.NotificationContainer.Type {
		case NotifTypePlaying:
			l.HandlePlaying(ctx, notif)
		case NotifTypeTranscodeUpdate:
			l.HandleTranscodeUpdate(notif)
		}
	}
}
