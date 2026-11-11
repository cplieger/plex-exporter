package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeHealth_passes_only_a_serving_listener(t *testing.T) {
	metrics := http.NotFoundHandler()
	for _, tc := range []struct {
		name    string
		serving bool
		want    int
	}{
		{"serving", true, 0},
		{"not serving", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(routes(metrics, fixedSignal(tc.serving)))
			defer ts.Close()
			if got := probeHealth(io.Discard, ts.Listener.Addr().String(), 2*time.Second); got != tc.want {
				t.Errorf("probeHealth = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("malformed LISTEN_ADDR", func(t *testing.T) {
		var stderr strings.Builder
		if got := probeHealth(&stderr, "9594", 2*time.Second); got != 1 {
			t.Errorf("probeHealth = %d for an address with no port separator, want 1", got)
		}
		if !strings.Contains(stderr.String(), `LISTEN_ADDR "9594"`) {
			t.Errorf("stderr = %q, want it to name LISTEN_ADDR and its value", stderr.String())
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Setup: listen: %v", err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		if got := probeHealth(io.Discard, addr, 2*time.Second); got != 1 {
			t.Errorf("probeHealth = %d with nothing listening, want 1", got)
		}
	})
}
