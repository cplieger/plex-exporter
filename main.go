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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/envx"
	"github.com/cplieger/health"
	"github.com/cplieger/plex-exporter/v2/internal/plex"
	"github.com/cplieger/plex-exporter/v2/internal/server"
	"github.com/cplieger/plexapi"
	"github.com/cplieger/slogx"
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
	slogx.Setup(slogx.Options{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Remove stale health file from a previous run that may have crashed
	// before its defer ran. Without this, the health probe would report
	// healthy before the initial Plex connection succeeds.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	serverAddr, err := envx.Require("PLEX_SERVER")
	if err != nil {
		slog.Error("startup config error", "error", err)
		return 1
	}
	plexToken, err := envx.Require("PLEX_TOKEN")
	if err != nil {
		slog.Error("startup config error", "error", err)
		return 1
	}
	listenAddr := envx.String("LISTEN_ADDRESS", ":9594")

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
	// Both routine machine paths ride the fleet-standard ProbeLogLevel --
	// scrapers hit /metrics on their own interval (60s in the reference
	// deployment) and the Docker HEALTHCHECK hits /api/health every 30s, so
	// a healthy probe logs at Debug (out of the shipped stream) while a
	// FAILING probe surfaces at Warn/Error with its status and request id
	// (the former skip idiom hid the failure signal along with the noise).
	// Logging and Recoverer default to slog.Default(), which is the logger the
	// rest of the app already uses.
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.ProbeLogLevel("/metrics", "/api/health")),
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

	slog.Info("starting metrics server", "addr", listener.Addr().String())
	// The pre-drain phase flips the health marker to unhealthy strictly
	// BEFORE webhttp.Run drains, so probes (Docker HEALTHCHECK + HTTP
	// /api/health) see red during the drain window — the marker is a FILE the
	// healthcheck CLI reads, which listener closure does not cover. (This was
	// a goroutine racing the drain start; WithPreDrain makes the ordering a
	// contract.) On a serve-error exit Run returns without invoking it, and
	// the deferred marker.Cleanup covers that path, unchanged.
	preDrain := webhttp.WithPreDrain(func(context.Context) {
		marker.Set(false)
		slog.Info("shutting down", "cause", context.Cause(ctx))
	})
	if srvErr := webhttp.Run(ctx, srv, listener, nil, webhttp.WithShutdownGrace(15*time.Second), preDrain); srvErr != nil {
		// A runtime Serve failure (not a clean shutdown): record it and exit
		// non-zero so the restart policy / exit-code alerting does not mistake
		// it for a graceful stop.
		slog.Error("metrics server failed", "error", srvErr)
		ps.RecordError("metrics_server")
		return 1
	}
	return 0
}

// isFatalStartupError reports whether an initial Plex Refresh error is a
// configuration or authentication problem that will not resolve without
// operator action (so run() should fail fast) rather than a transient
// connectivity failure (so run() should start degraded and let
// RunRefreshLoop recover). A bad token (401/403) or other 4xx, a 404, and
// TLS/certificate errors are fatal; dial/DNS/timeout errors and 5xx
// responses (a Plex still starting up) are treated as transient.
func isFatalStartupError(err error) bool {
	// Plex returned an HTTP status: the shared classifier treats a 4xx —
	// except 429 (rate limit, retried with Retry-After honored) and 408
	// (request timeout, same class as the transport timeouts below) — as
	// fatal config/auth, and a 5xx (Plex up but not ready) as transient.
	if plexapi.IsConfigError(err) {
		return true
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
