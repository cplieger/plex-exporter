package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/go/httpserver/v2"
)

type fixedSignal bool

func (s fixedSignal) Healthy() bool { return bool(s) }

// serveOps starts the listener run() builds, over a 127.0.0.1 port, and returns
// its address. hosts is ALLOWED_HOSTS' entries.
func serveOps(t *testing.T, serving fixedSignal, hosts ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ops, err := httpserver.ListenOps(ctx, httpserver.OpsConfig{
		Metrics:      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		Healthz:      healthzHandler(serving),
		Setting:      "LISTEN_ADDR",
		Addr:         "127.0.0.1:0",
		HostsSetting: "ALLOWED_HOSTS",
		Hosts:        hosts,
	})
	if err != nil {
		cancel()
		t.Fatalf("Setup: ListenOps: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- httpserver.RunWithOps(ctx, ops) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("RunWithOps = %v", err)
		}
	})
	return ops.Addr().String()
}

// request answers the status, headers and body of one call, so no caller holds
// an open response.
func request(t *testing.T, method, addr, path, host string) (status int, header http.Header, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(raw)
}

func TestHealthzHandler(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		wantReasons []string
		serving     bool
		wantStatus  int
		wantJSON    string
	}{
		{name: "GET while serving", method: http.MethodGet, path: "/healthz", serving: true, wantStatus: http.StatusOK, wantJSON: "ok"},
		{name: "GET while not serving", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusServiceUnavailable, wantJSON: "unhealthy", wantReasons: []string{"plex-exporter is starting or shutting down"}},
		{name: "HEAD while serving", method: http.MethodHead, path: "/healthz", serving: true, wantStatus: http.StatusOK},
		{name: "POST is refused", method: http.MethodPost, path: "/healthz", serving: true, wantStatus: http.StatusMethodNotAllowed},
		{name: "the old path is gone", method: http.MethodGet, path: "/api/health", serving: true, wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, header, body := request(t, tc.method, serveOps(t, fixedSignal(tc.serving)), tc.path, "")
			if status != tc.wantStatus {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, status, tc.wantStatus)
			}
			if tc.wantJSON == "" {
				return
			}
			for name, want := range map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"} {
				if got := header.Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			var got struct {
				Status  string   `json:"status"`
				Reasons []string `json:"reasons"`
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("body %q is not JSON: %v", body, err)
			}
			if got.Status != tc.wantJSON || !slices.Equal(got.Reasons, tc.wantReasons) {
				t.Errorf("body = %q, want status %q and reasons %q", body, tc.wantJSON, tc.wantReasons)
			}
		})
	}
}

func TestAllowedHosts(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", "")
	if got := allowedHosts(); slices.ContainsFunc(got, func(h string) bool { return strings.TrimSpace(h) != "" }) {
		t.Errorf("unset ALLOWED_HOSTS = %q, want no entry", got)
	}
	t.Setenv("ALLOWED_HOSTS", "plex-exporter, scrape.example")
	if got := allowedHosts(); len(got) != 2 {
		t.Errorf("ALLOWED_HOSTS = %q, want two entries", got)
	}
	addr := serveOps(t, true)
	if status, _, _ := request(t, http.MethodGet, addr, "/healthz", "anything.example"); status != http.StatusOK {
		t.Errorf("an unset ALLOWED_HOSTS refused a foreign Host: %d, want every Host served", status)
	}
	addr = serveOps(t, true, "plex-exporter")
	for host, want := range map[string]int{"plex-exporter": http.StatusOK, "attacker.example": http.StatusForbidden} {
		status, _, body := request(t, http.MethodGet, addr, "/metrics", host)
		if status != want {
			t.Errorf("Host %q = %d, want %d", host, status, want)
		}
		if want == http.StatusForbidden && !strings.Contains(body, "host_not_allowed") {
			t.Errorf("refusal body %q lacks the host_not_allowed code", body)
		}
	}
}
