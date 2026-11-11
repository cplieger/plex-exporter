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
	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/measure"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/server"
	"github.com/cplieger/plexapi/v3"
)

// healthzPath is the health route; the image's healthcheck and the
// documented probes ask it.
const healthzPath = "/healthz"

const notServingReason = "plex-exporter is starting or shutting down"

// healthProbeTimeout sits inside the Dockerfile's 5s HEALTHCHECK --timeout, so
// a hung listener fails the check with a written reason instead of a kill.
const healthProbeTimeout = 4 * time.Second

// version is the release this binary was built as, set with
// -ldflags "-X main.version=<tag>"; a build without it reports "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(probeHealth(os.Stderr, listenAddr(), healthProbeTimeout))
	}
	os.Exit(run())
}

func listenAddr() string {
	return cmp.Or(envx.String("LISTEN_ADDR"), ":9594")
}

// newRegistry builds the /metrics registry: the exporter's families, and the
// Go runtime and process families every measure registry serves.
func newRegistry(ps *server.Server) *measure.Registry {
	reg := measure.NewRegistry("")
	reg.MustRegister(ps.Collector(version))
	return reg
}

// metricsHandler serves reg's page, gzipped for a scraper that asks.
func metricsHandler(reg *measure.Registry) http.Handler {
	return httpserver.Gzip(httpserver.WithGzipTypes("text/plain"))(reg.Handler())
}

// routes serves /metrics and GET /healthz, which reports serving.
func routes(metrics http.Handler, serving health.Signal) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.Handle("GET "+healthzPath, health.CheckHandler(health.Check{Signal: serving, Reason: notServingReason}))

	// Chain is outermost-first: Logging records the final status (even a
	// recovered panic), Recoverer converts a panic to its 500 before
	// SecurityHeaders' baseline is set. No CSP/HSTS: both endpoints are
	// machine-scraped, non-browser.
	return httpserver.Chain(mux,
		httpserver.Logging(httpserver.WithProbeLogLevel("/metrics", healthzPath)),
		httpserver.Recoverer(),
		httpserver.SecurityHeaders(),
	)
}

func run() int {
	slogx.Setup(slogx.Options{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	slog.Info("starting plex-exporter", "version", version,
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

	go ps.RunRefreshLoop(ctx)

	st := health.NewStatus()
	handler := routes(metricsHandler(newRegistry(ps)), st)

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

	st.Set(true)

	go ps.Sessions.RunPruneLoop(ctx)
	go ps.RunSessionPollLoop(ctx)
	go ps.RunHistoryLoop(ctx)
	go ps.RunLibraryWalkLoop(ctx)

	slog.Info("starting metrics server", "addr", listener.Addr().String())
	// WithPreDrain marks the app unhealthy before httpserver.Run drains, so a
	// probe answered during the drain reads 503.
	preDrain := httpserver.WithPreDrain(func(context.Context) {
		st.BeginDrain()
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
