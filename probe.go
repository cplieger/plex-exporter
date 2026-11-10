package main

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/cplieger/go/health/probe"
)

// healthzURL is the /healthz address the image's healthcheck asks: the listen
// port on loopback, or on the bound host when LISTEN_ADDR names one, since a
// listener bound to one address answers on no other.
func healthzURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", err
	}
	if ip, perr := netip.ParseAddr(host); host == "" || (perr == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + healthzPath, nil
}

// probeHealth is `plex-exporter health`: it exits 0 only when /healthz
// answers 2xx within timeout, so a listener that no longer answers fails.
func probeHealth(stderr io.Writer, listenAddr string, timeout time.Duration) int {
	url, err := healthzURL(listenAddr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "unhealthy: LISTEN_ADDR %q: %v\n", listenAddr, err)
		return 1
	}
	return probe.Check(stderr, timeout, url)
}
