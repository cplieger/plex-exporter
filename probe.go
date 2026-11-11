package main

import (
	"fmt"
	"io"
	"time"

	"github.com/cplieger/go/health/v2/probe"
)

// probeHealth is `plex-exporter health`: it exits 0 only when /healthz
// answers 2xx within timeout, so a listener that no longer answers fails.
func probeHealth(stderr io.Writer, listenAddr string, timeout time.Duration) int {
	url, err := probe.LocalURL(listenAddr, healthzPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "unhealthy: LISTEN_ADDR %q: %v\n", listenAddr, err)
		return 1
	}
	return probe.Check(stderr, timeout, url)
}
