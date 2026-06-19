// Package main is the composition root for plex-exporter. Wiring
// only: env parsing, concrete-type construction from internal/*
// packages, HTTP listener, and goroutine launch. All business logic
// lives in internal/{plex,plexapi,metrics,library,sessions,server};
// see those packages for behaviour.
package main

import (
	"context"
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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
		slog.Error("invalid PLEX_SERVER URL", "error", err)
		return 1
	}
	ps := server.NewServer(client)

	if refreshErr := ps.Refresh(ctx); refreshErr != nil {
		slog.Error("cannot connect to plex server", "error", refreshErr)
		ps.RecordError("refresh")
		return 1
	}
	ps.SetHTTPReachable(true)
	ps.SetSessionsReachable(true)
	slog.Info("connected to plex server",
		"name", ps.Name, "version", ps.Version,
		"libraries", len(ps.Libraries))

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
			cancel()
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
