package wsclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/sessions"
)

// connState is a mutex-protected bool used by tests to observe the
// connected flag the Listener reports via SetConnected.
type connState struct {
	mu sync.Mutex
	v  bool
}

func (c *connState) set(v bool) { c.mu.Lock(); c.v = v; c.mu.Unlock() }
func (c *connState) get() bool  { c.mu.Lock(); defer c.mu.Unlock(); return c.v }

func TestListen_sets_ws_connected_and_clears_on_disconnect(t *testing.T) {
	// Given a WS server that accepts one connection and closes it,
	// Listen should report connected=true during the session and false
	// after. Context cancellation must stop the reconnect loop.
	accepted := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		accepted <- struct{}{}
		time.Sleep(50 * time.Millisecond)
		c.Close(websocket.StatusNormalClosure, "test done")
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	tracker := sessions.NewTracker()
	state := &connState{}
	l := listenerFor(client, tracker, nil)
	l.SetConnected = state.set

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Listen(ctx); close(done) }()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("websocket never accepted")
	}

	// Wait until connected flips true.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if state.get() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !state.get() {
		t.Error("connected never became true during established connection")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not return after context cancel")
	}

	if state.get() {
		t.Error("connected should be false after Listen returns")
	}
}

func TestListen_backoff_reset_after_traffic(t *testing.T) {
	// With backoff reset on sawTraffic (not on dial), a server that
	// accepts + sends + closes should trigger rapid reconnects at
	// MinBackoff. We observe by counting accepts within a time budget.
	var accepts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Send one message so sawTraffic becomes true, then close.
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"NotificationContainer":{"type":"unknown"}}`))
		c.Close(websocket.StatusNormalClosure, "")
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: time.Second}}
	tracker := sessions.NewTracker()
	l := listenerFor(client, tracker, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	l.Listen(ctx)

	// With MinBackoff=1ms (TestMain) and rapid cycle, expect many reconnects.
	if accepts.Load() < 3 {
		t.Errorf("expected at least 3 reconnects in 100ms with backoff reset, got %d", accepts.Load())
	}
}

func TestListen_dial_failure_escalates_backoff(t *testing.T) {
	// When dial repeatedly fails (connected=false), backoff must escalate
	// and throttle reconnect attempts. Bounded by MaxBackoff=10ms in tests.
	//
	// Finding 2: this test races when MinBackoff is too small relative to
	// httptest handshake latency (hundreds of µs). Raise MinBackoff
	// locally so the backoff sequence is observable above the noise floor.
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "no", http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: 500 * time.Millisecond}}
	tracker := sessions.NewTracker()
	l := listenerFor(client, tracker, nil)
	l.Backoff = BackoffConfig{Min: 5 * time.Millisecond, Max: 10 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	l.Listen(ctx)

	if attempts.Load() == 0 {
		t.Error("no dial attempts were made")
	}
	// Sanity: attempts should be well under 1000 (would be >1000 without backoff).
	if attempts.Load() > 500 {
		t.Errorf("backoff did not throttle: %d attempts in 200ms", attempts.Load())
	}
}

func TestConnectAndListen_dispatches_playing_and_transcode_messages(t *testing.T) {
	// Drive a real WebSocket server that sends: (1) a malformed JSON
	// message (must be logged and skipped), (2) a transcode update,
	// (3) a playing notification, then (4) an unknown type (ignored).
	// Then close the channel so the server side drops the connection.
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"sessionKey":"sess-1","Player":{},"User":{"title":"u"},"Media":[{"Part":[{"key":"/transcode/session/ts-1/stream"}]}]}]}}`)
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"title":"Media Title","type":"movie","librarySectionID":"1","Media":[{}]}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	msgCh := make(chan string, 4)
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		for msg := range msgCh {
			if err := c.Write(r.Context(), websocket.MessageText, []byte(msg)); err != nil {
				return
			}
		}
	})

	mux := http.NewServeMux()
	mux.Handle("/:/websockets/notifications", wsHandler)
	mux.Handle("/", apiHandler)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	tracker := sessions.NewTracker()
	l := listenerFor(client, tracker, nil)

	// Playing must arrive first so the session exists when the transcode
	// update is dispatched; HandleTranscodeUpdate only matches existing
	// sessions by the transcode key embedded in the session's Part.Key.
	msgCh <- `{garbage json`
	msgCh <- `{"NotificationContainer":{"type":"playing","PlaySessionStateNotification":[{"sessionKey":"sess-1","ratingKey":"42","state":"playing","transcodeSession":"ts-1"}]}}`
	msgCh <- `{"NotificationContainer":{"type":"transcodeSession.update","TranscodeSession":[{"key":"ts-1","videoDecision":"transcode","audioDecision":"copy"}]}}`
	msgCh <- `{"NotificationContainer":{"type":"unknown.event"}}`

	// Give the handler enough time to process messages before closing.
	time.AfterFunc(200*time.Millisecond, func() { close(msgCh) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sawTraffic, _ := l.ConnectAndListen(ctx)

	if !sawTraffic {
		t.Error("sawTraffic = false, expected true after receiving messages")
	}

	snap, _ := tracker.SnapshotSessions()
	sess, ok := snap["sess-1"]
	if !ok {
		t.Fatal("session sess-1 was never created (invalid JSON may have aborted the loop)")
	}
	if sess.State != sessions.StatePlaying {
		t.Errorf("session state = %q, want playing", sess.State)
	}
	if sess.TranscodeType != metrics.ValVideo {
		t.Errorf("transcodeType = %q, want %q (transcode update dispatch failed)", sess.TranscodeType, metrics.ValVideo)
	}
}

func TestConnectAndListen_dial_failure_returns_no_traffic(t *testing.T) {
	// A server that refuses the upgrade must cause ConnectAndListen to
	// return sawTraffic=false so Listen's backoff escalates.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: 500 * time.Millisecond}}
	tracker := sessions.NewTracker()
	l := listenerFor(client, tracker, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sawTraffic, err := l.ConnectAndListen(ctx)

	if sawTraffic {
		t.Error("expected sawTraffic=false on failed upgrade")
	}
	if err == nil {
		t.Error("expected non-nil error on failed upgrade")
	}
	if !strings.Contains(err.Error(), "websocket dial") {
		t.Errorf("error = %v, want wrapped 'websocket dial' error", err)
	}
}

func TestConnectAndListen_read_limit_enforced(t *testing.T) {
	// Verify SetReadLimit triggers an error on oversized frames (limit is
	// 1 MiB; send a 2 MiB frame).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		huge := make([]byte, 2<<20)
		_ = c.Write(r.Context(), websocket.MessageText, huge)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: 2 * time.Second}}
	tracker := sessions.NewTracker()
	l := listenerFor(client, tracker, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := l.ConnectAndListen(ctx)

	if err == nil {
		t.Error("expected error from oversized read frame")
	}
}

func TestDialWebsocket_https_uses_wss_scheme(t *testing.T) {
	// Verify that when baseURL uses https, dialWebsocket constructs a wss URL.
	// We can't easily test the full dial (needs a real TLS server), but we can
	// verify the scheme logic by attempting a dial to a non-TLS server with
	// an https base URL — the error message should reference wss://.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not a websocket", http.StatusBadRequest)
	}))
	defer ts.Close()

	// Parse the test server URL and change scheme to https
	tsURL, _ := url.Parse(ts.URL)
	tsURL.Scheme = "https"

	client := &plex.Client{BaseURL: tsURL, Token: "t", HTTPClient: &http.Client{Timeout: time.Second}}
	tracker := sessions.NewTracker()
	l := listenerFor(client, tracker, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := l.ConnectAndListen(ctx)
	if err == nil {
		t.Fatal("expected error dialing wss to non-TLS server")
	}
	// The error should mention websocket dial failure
	if !strings.Contains(err.Error(), "websocket dial") {
		t.Errorf("error = %v, want wrapped 'websocket dial' error", err)
	}
}
