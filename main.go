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
	// Embed the IANA tz database so TZ (default Europe/Paris) is honored even
	// though the distroless static base ships no /usr/share/zoneinfo; without
	// it time.Local silently falls back to UTC.
	_ "time/tzdata"

	"github.com/cplieger/health"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// errMetricsServer is the cancellation cause set when the metrics HTTP server
// fails at runtime, so run() exits non-zero (a crash must not look like a clean
// shutdown to the restart policy or exit-code alerting).
var errMetricsServer = errors.New("metrics server failed")

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
	}
	os.Exit(run())
}

func run() int {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(sigCtx)
	defer cancel(nil)

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
			// (the Serve goroutine that could cancel with errMetricsServer has not started
			// yet); the inner Refresh deadline leaves the parent ctx.Err() nil, so a genuinely
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

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	// Bind the listener before marking healthy so a port-in-use failure is
	// reported before Docker's healthcheck can observe a stale-true state.
	lc := &net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		slog.Error("cannot bind listen address", "addr", listenAddr, "error", err)
		return 1
	}
	go func() {
		slog.Info("starting metrics server", "addr", listener.Addr().String())
		if srvErr := httpServer.Serve(listener); !errors.Is(srvErr, http.ErrServerClosed) {
			slog.Error("metrics server failed", "error", srvErr)
			ps.RecordError("metrics_server")
			cancel(errMetricsServer)
		}
	}()

	marker.Set(true)

	go ps.Sessions.RunPruneLoop(ctx)
	go ps.RunSessionPollLoop(ctx)

	// Block until context is cancelled (SIGINT/SIGTERM).
	<-ctx.Done()

	// Flip the health marker to unhealthy before Shutdown drains so probes
	// (Docker HEALTHCHECK + HTTP /api/health) see red during the drain window.
	marker.Set(false)
	slog.Info("shutting down", "cause", context.Cause(ctx))
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown error", "error", err)
	}
	if errors.Is(context.Cause(ctx), errMetricsServer) {
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
