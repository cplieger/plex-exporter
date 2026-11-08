// Package main is the composition root for plex-exporter: env parsing,
// construction, the HTTP listener and goroutine launch. Business logic
// lives under internal/.
package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/server"
	"github.com/cplieger/plexapi/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// healthzPath is the health route; the image's healthcheck and the
// documented probes ask it.
const healthzPath = "/healthz"

// notServingReason is what /healthz reports while the marker is absent.
const notServingReason = "plex-exporter is not serving: it is starting or shutting down, or cannot write its health marker in /tmp"

// healthProbeTimeout sits inside the Dockerfile's 5s HEALTHCHECK --timeout, so
// a hung listener fails the check with a written reason instead of a kill.
const healthProbeTimeout = 4 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(probeHealth(os.Stderr, listenAddr(), healthProbeTimeout))
	}
	os.Exit(run())
}

func listenAddr() string {
	return cmp.Or(envx.String("LISTEN_ADDR"), ":9594")
}

// routes serves /metrics and GET /healthz, which reports serving.
func routes(metrics http.Handler, serving health.Signal) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.Handle("GET "+healthzPath, health.CheckHandler(health.Check{Signal: serving, Reason: notServingReason}))

	// Chain is outermost-first: Logging records the final status (even a
	// recovered panic), Recoverer converts a panic to its 500 before
	// SecurityHeaders' baseline is set. No CSP/HSTS: both endpoints are
	// machine-scraped, non-browser. ProbeLogLevel keeps a healthy probe at
	// Debug and surfaces a failing one at Warn/Error with status + request id.
	return httpserver.Chain(mux,
		httpserver.Logging(httpserver.ProbeLogLevel("/metrics", healthzPath)),
		httpserver.Recoverer(),
		httpserver.SecurityHeaders(),
	)
}

func run() int {
	slogx.Setup(slogx.Options{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Clear a marker a crashed run left, so /healthz reports serving only
	// once this run has bound its listener.
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
	listenAddr := listenAddr()

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
		// Every Plex failure starts degraded: plex_http_reachable 0 and the
		// refresh error count carry it until RunRefreshLoop reaches Plex.
		// The level tells the reader whether to act.
		if operatorFixable(refreshErr) {
			slog.Error("cannot connect to plex server", "error", refreshErr)
		} else {
			slog.Warn("initial plex connection failed; starting in degraded state", "error", refreshErr)
		}
		ps.StartDegraded(refreshErr)
	} else {
		ps.SetHTTPReachable(true)
		ps.SetSessionsReachable(true)
		slog.Info("connected to plex server",
			"name", ps.Name, "version", ps.Version,
			"libraries", len(ps.Libraries))
	}

	prometheus.MustRegister(ps)
	go ps.RunRefreshLoop(ctx)

	handler := routes(promhttp.Handler(), marker)

	// ReadTimeout/WriteTimeout guard against slow-body/slow-write DoS; New
	// leaves them unset by default.
	srv := httpserver.New(handler)
	srv.ReadHeaderTimeout = 5 * time.Second
	srv.ReadTimeout = 5 * time.Second
	srv.WriteTimeout = 10 * time.Second

	lc := &net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		slog.Error("cannot bind listen address", "addr", listenAddr, "error", err)
		return 1
	}

	marker.Set(true)

	go ps.Sessions.RunPruneLoop(ctx)
	go ps.RunSessionPollLoop(ctx)
	go ps.RunHistoryLoop(ctx)
	go ps.RunLibraryWalkLoop(ctx)

	slog.Info("starting metrics server", "addr", listener.Addr().String())
	// WithPreDrain marks the app unhealthy before httpserver.Run drains, so a
	// probe answered during the drain reads 503. On a serve-error exit Run
	// skips it; the deferred marker.Cleanup covers that path.
	preDrain := httpserver.WithPreDrain(func(context.Context) {
		marker.Set(false)
		slog.Info("shutting down", "cause", context.Cause(ctx))
	})
	if srvErr := httpserver.Run(ctx, srv, listener, httpserver.WithShutdownGrace(15*time.Second), preDrain); srvErr != nil {
		slog.Error("metrics server failed", "error", srvErr)
		ps.RecordError("metrics_server")
		return 1
	}
	return 0
}

// operatorFixable reports whether an initial Plex Refresh error will not
// resolve without operator action (bad token/URL, a 404, or a TLS failure
// httpclient.CauseOf names: a certificate, a fatal alert or a plain-HTTP
// answer) versus a transient connectivity failure (dial/DNS/timeout, or a
// 5xx).
func operatorFixable(err error) bool {
	if plexapi.IsConfigError(err) {
		return true
	}
	if errors.Is(err, plex.ErrNotFound) {
		return true
	}
	return httpclient.CauseOf(err) == httpclient.CauseTLS
}
