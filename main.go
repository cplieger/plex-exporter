// Package main is the composition root for plex-exporter. Wiring
// only: env parsing, concrete-type construction from internal/*
// packages, HTTP listener, and goroutine launch. All business logic
// lives in internal/{plex,plexapi,metrics,library,sessions,server};
// see those packages for behaviour.
package main

import (
	"cmp"
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

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/health"
	"github.com/cplieger/plex-exporter/v2/internal/plex"
	"github.com/cplieger/plex-exporter/v2/internal/server"
	"github.com/cplieger/plexapi/v2"
	"github.com/cplieger/slogx"
	"github.com/cplieger/webhttp/v2"
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

	// Clear a health marker left by a crashed previous run so the probe
	// does not report healthy before the initial Plex connection succeeds.
	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	plexURL, err := envx.Require("PLEX_URL")
	if err != nil {
		slog.Error("startup config error", "error", err)
		return 1
	}
	// envx.Secret: the token may arrive via PLEX_TOKEN_FILE, keeping it out
	// of the container environment and docker inspect.
	plexToken, err := envx.Secret("PLEX_TOKEN")
	if err != nil {
		slog.Error("startup config error", "error", err)
		return 1
	}
	listenAddr := cmp.Or(envx.String("LISTEN_ADDR"), ":9594")

	caCertPath := envx.String("PLEX_CA_CERT_PATH")
	slog.Info("starting plex-exporter",
		"server", plexURL, "listen", listenAddr,
		"ca_cert_path", caCertPath)

	client, err := plex.NewClient(plex.Options{ServerURL: plexURL, Token: plexToken, CACertPath: caCertPath})
	if err != nil {
		slog.Error("cannot create plex client", "error", err)
		return 1
	}
	ps := server.New(client)

	if refreshErr := ps.Refresh(ctx); refreshErr != nil {
		if ctx.Err() != nil {
			// ctx.Err() is non-nil here only on signal cancellation (the metrics
			// server has not started yet, so nothing else can cancel ctx); the
			// inner Refresh deadline leaves the parent ctx.Err() nil, so a
			// genuinely slow Plex still falls through to the degraded-start path.
			slog.Info("shutdown requested during startup", "cause", context.Cause(ctx))
			return 0
		}
		if isFatalStartupError(refreshErr) {
			slog.Error("cannot connect to plex server", "error", refreshErr)
			ps.RecordError("refresh")
			return 1
		}
		// Transient failure: start degraded rather than crash-loop. RunRefreshLoop
		// recovers and flips plex_http_reachable to 1 once Plex is reachable.
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

	// Chain is outermost-first: Logging records the final status (even a
	// recovered panic), Recoverer converts a panic to its 500 before
	// SecurityHeaders' baseline is set. No CSP/HSTS: both endpoints are
	// machine-scraped, non-browser. ProbeLogLevel keeps a healthy probe at
	// Debug and surfaces a failing one at Warn/Error with status + request id.
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.ProbeLogLevel("/metrics", "/api/health")),
		webhttp.Recoverer(),
		webhttp.SecurityHeaders(),
	)

	// ReadTimeout/WriteTimeout guard against slow-body/slow-write DoS; NewServer
	// leaves them unset by default.
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
	// WithPreDrain flips the health marker to unhealthy strictly BEFORE
	// webhttp.Run drains, so probes see red during the drain window (the
	// marker is a FILE the healthcheck CLI reads; listener closure alone does
	// not cover it). On a serve-error exit Run skips it; deferred marker.Cleanup
	// covers that path.
	preDrain := webhttp.WithPreDrain(func(context.Context) {
		marker.Set(false)
		slog.Info("shutting down", "cause", context.Cause(ctx))
	})
	if srvErr := webhttp.Run(ctx, srv, listener, nil, webhttp.WithShutdownGrace(15*time.Second), preDrain); srvErr != nil {
		slog.Error("metrics server failed", "error", srvErr)
		ps.RecordError("metrics_server")
		return 1
	}
	return 0
}

// isFatalStartupError reports whether an initial Plex Refresh error will
// not resolve without operator action (bad token/URL, a 404, or a
// TLS/certificate error) versus a transient connectivity failure that
// RunRefreshLoop can recover from (dial/DNS/timeout, or a 5xx).
func isFatalStartupError(err error) bool {
	if plexapi.IsConfigError(err) {
		return true
	}
	if errors.Is(err, plex.ErrNotFound) {
		return true
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	return false
}
