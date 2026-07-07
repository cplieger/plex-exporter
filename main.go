// Package main is the composition root for plex-exporter. Wiring
// only: env parsing, concrete-type construction from internal/*
// packages, HTTP listener, and goroutine launch. All business logic
// lives in internal/{plex,plexapi,metrics,library,sessions,server};
// see those packages for behaviour.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/server"
	"github.com/cplieger/webhttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
	}
	os.Exit(run())
}

func run() int {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: utcTimeAttr})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Remove stale health file from a previous run that may have crashed
	// before its defer ran. Without this, the health probe would report
	// healthy before the initial Plex connection succeeds.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	serverAddr, err := requireEnv("PLEX_SERVER")
	if err != nil {
		slog.Error("startup config error", "error", err)
		return 1
	}
	plexToken, err := requireEnv("PLEX_TOKEN")
	if err != nil {
		slog.Error("startup config error", "error", err)
		return 1
	}
	listenAddr := envOr("LISTEN_ADDRESS", ":9594")

	caCertPath := os.Getenv("PLEX_CA_CERT_PATH")
	slog.Info("starting plex-exporter",
		"server", serverAddr, "listen", listenAddr,
		"ca_cert_path", caCertPath)

	client, err := plex.NewClient(serverAddr, plexToken, caCertPath)
	if err != nil {
		slog.Error("cannot create plex client", "error", err)
		return 1
	}
	ps := server.NewServer(client)

	if refreshErr := ps.Refresh(ctx); refreshErr != nil {
		if ctx.Err() != nil {
			// Shutdown (SIGINT/SIGTERM) arrived during the initial connect -- not a Plex
			// failure. Exit cleanly instead of logging a misleading "degraded state" warning
			// for a cancelled startup. ctx.Err() is non-nil here only on signal cancellation
			// (the metrics server has not started yet, so nothing else can cancel ctx);
			// the inner Refresh deadline leaves the parent ctx.Err() nil, so a genuinely
			// slow Plex still degrades.
			slog.Info("shutdown requested during startup", "cause", context.Cause(ctx))
			return 0
		}
		if isFatalStartupError(refreshErr) {
			// Bad token/URL, another 4xx, or a TLS/cert misconfiguration: Plex
			// answered or the config is wrong, so this will not resolve on its
			// own. Fail fast with a precise signal.
			slog.Error("cannot connect to plex server", "error", refreshErr)
			ps.RecordError("refresh")
			return 1
		}
		// Transient connectivity failure (dial/DNS/timeout, or a 5xx from a Plex
		// still starting up): start in a degraded state instead of crash-looping.
		// Bind /metrics and report plex_http_reachable=0 so the outage stays
		// observable; RunRefreshLoop recovers and flips the gauge to 1 once Plex
		// is reachable again.
		slog.Warn("initial plex connection failed; starting in degraded state", "error", refreshErr)
		ps.RecordError("refresh")
		ps.SetHTTPReachable(false)
	} else {
		ps.SetHTTPReachable(true)
		ps.SetSessionsReachable(true)
		slog.Info("connected to plex server",
			"name", ps.Name, "version", ps.Version,
			"libraries", len(ps.Libraries))
	}

	prometheus.MustRegister(ps)
	go ps.RunRefreshLoop(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/api/health", health.Handler(marker))

	// Standard webhttp middleware chain. Chain is outermost-first, so this is
	// the fleet's canonical order: Logging outermost (its access line records
	// the final status), Recoverer inside it (a recovered panic is logged as
	// its 500, not the StatusRecorder's default 200), and SecurityHeaders
	// innermost (its nosniff / X-Frame-Options: DENY / Referrer-Policy baseline
	// is set before the handler runs, so it survives even onto a recovered
	// 500). No CSP or HSTS: /metrics and /api/health are non-browser,
	// machine-scraped endpoints, so nosniff is the header that earns its keep.
	//
	// Both routine machine paths are skipped from the access line -- Prometheus
	// scrapes /metrics roughly every 15s and the Docker HEALTHCHECK hits
	// /api/health every 30s, so logging either would flood the log for no
	// operational gain. The request id is still minted, echoed, and threaded on
	// skipped paths, so a panic in either handler is still logged with its id,
	// and any unexpected path (a 404 from a stray client) is still logged.
	// Logging and Recoverer default to slog.Default(), which is the logger the
	// rest of the app already uses.
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.WithSkipPaths("/metrics", "/api/health")),
		webhttp.Recoverer(),
		webhttp.SecurityHeaders(),
	)

	// webhttp.NewServer's defaults already supply IdleTimeout 120s and
	// MaxHeaderBytes 1 MiB, so only the three timeouts the exporter needs are
	// passed: it tightens ReadHeaderTimeout to 5s and, because /metrics is a
	// bounded non-streaming response, sets the ReadTimeout/WriteTimeout that
	// NewServer leaves unset by default (omitting them would drop today's
	// slow-body/slow-write DoS protection).
	srv := webhttp.NewServer(handler,
		webhttp.WithReadHeaderTimeout(5*time.Second),
		webhttp.WithReadTimeout(5*time.Second),
		webhttp.WithWriteTimeout(10*time.Second),
	)

	// Bind the listener before marking healthy so a port-in-use failure is
	// reported before Docker's healthcheck can observe a stale-true state.
	lc := &net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		slog.Error("cannot bind listen address", "addr", listenAddr, "error", err)
		return 1
	}

	marker.Set(true)

	go ps.Sessions.RunPruneLoop(ctx)
	go ps.RunSessionPollLoop(ctx)

	// Flip the health marker to unhealthy the moment shutdown is signalled,
	// before webhttp.Run drains, so probes (Docker HEALTHCHECK + HTTP
	// /api/health) see red during the drain window (Run's teardown runs only
	// after the drain completes).
	go func() {
		<-ctx.Done()
		marker.Set(false)
		slog.Info("shutting down", "cause", context.Cause(ctx))
	}()

	slog.Info("starting metrics server", "addr", listener.Addr().String())
	if srvErr := webhttp.Run(ctx, srv, listener, nil, webhttp.WithShutdownGrace(15*time.Second)); srvErr != nil {
		// A runtime Serve failure (not a clean shutdown): record it and exit
		// non-zero so the restart policy / exit-code alerting does not mistake
		// it for a graceful stop.
		slog.Error("metrics server failed", "error", srvErr)
		ps.RecordError("metrics_server")
		return 1
	}
	return 0
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s environment variable must be specified", key)
	}
	return v, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// isFatalStartupError reports whether an initial Plex Refresh error is a
// configuration or authentication problem that will not resolve without
// operator action (so run() should fail fast) rather than a transient
// connectivity failure (so run() should start degraded and let
// RunRefreshLoop recover). A bad token (401/403) or other 4xx, a 404, and
// TLS/certificate errors are fatal; dial/DNS/timeout errors and 5xx
// responses (a Plex still starting up) are treated as transient.
func isFatalStartupError(err error) bool {
	// Plex returned an HTTP status: a 4xx means it reached us and rejected the
	// request (bad token or wrong endpoint); a 5xx means it is up but not ready
	// yet, which can clear on its own.
	var statusErr *plex.HTTPStatusError
	if errors.As(err, &statusErr) {
		// 429 (Too Many Requests) and 408 (Request Timeout) are rate-limit / timeout signals, not
		// config/auth errors: the retry round-tripper already treats 429 as transient (retries it,
		// honoring Retry-After), and a request timeout is the same class as the transport timeouts
		// already handled as transient below. Treat them as transient at startup too, so a
		// throttling/slow Plex starts degraded and backs off rather than exiting and crash-looping
		// under the restart policy.
		if statusErr.Code == http.StatusTooManyRequests || statusErr.Code == http.StatusRequestTimeout {
			return false
		}
		return statusErr.Code < 500
	}
	// 404 on the providers/identity endpoint: reached Plex, wrong server.
	if errors.Is(err, plex.ErrNotFound) {
		return true
	}
	// TLS/certificate misconfiguration (e.g. a self-signed cert without
	// PLEX_CA_CERT_PATH): will not recover without a config change.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	// Transport errors (connection refused, DNS failure, timeout): Plex is
	// unreachable now but may come back.
	return false
}

// utcTimeAttr is a slog ReplaceAttr that renders the record's built-in time
// key in UTC, so log-line timestamps are zone-stable regardless of the
// container's TZ (the fleet logs-in-UTC standard). It rewrites only the
// top-level time attribute; a user attribute that happens to share the "time"
// key inside a group is left untouched.
func utcTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}
