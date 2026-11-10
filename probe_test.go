package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthzURL(t *testing.T) {
	cases := map[string]string{
		":9594":          "http://127.0.0.1:9594/healthz",
		"0.0.0.0:9594":   "http://127.0.0.1:9594/healthz",
		"[::]:9594":      "http://127.0.0.1:9594/healthz",
		"192.0.2.5:9000": "http://192.0.2.5:9000/healthz",
		"[::1]:9594":     "http://[::1]:9594/healthz",
	}
	for addr, want := range cases {
		got, err := healthzURL(addr)
		if err != nil || got != want {
			t.Errorf("healthzURL(%q) = %q, %v, want %q", addr, got, err, want)
		}
	}
	if _, err := healthzURL("9594"); err == nil {
		t.Error(`healthzURL("9594") succeeded, want an error for an address with no port separator`)
	}
}

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
