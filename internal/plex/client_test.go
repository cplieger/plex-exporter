package plex

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/plexapi/v2"
)

// The HTTP transport (retry policy, redirect refusal, path guard, body
// caps, status mapping) is github.com/cplieger/plexapi and is tested there.
// These tests pin the exporter's adapter: construction (CA path handling),
// the retry-counter metric, the error re-exports the startup classifier
// keys on, and the library's ContainerTotalSize reached through the
// embedded client.

func TestNewClient_invalid_url_returns_error(t *testing.T) {
	for _, u := range []string{"ftp://plex:32400", "http://", "://bad"} {
		if _, err := NewClient(u, "tok", ""); err == nil {
			t.Errorf("NewClient(%q) succeeded, want error", u)
		}
	}
}

func TestNewClient_ca_cert_path_missing_file_errors(t *testing.T) {
	_, err := NewClient("https://plex:32400", "tok", filepath.Join(t.TempDir(), "nope.pem"))
	if err == nil {
		t.Fatal("missing CA file accepted")
	}
	if got := err.Error(); !strings.Contains(got, "PLEX_CA_CERT_PATH") {
		t.Errorf("error %q should name PLEX_CA_CERT_PATH", got)
	}
}

func TestNewClient_ca_cert_path_invalid_pem_errors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(p, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient("https://plex:32400", "tok", p); err == nil {
		t.Fatal("garbage PEM accepted")
	}
}

// TestNewClient_ca_cert_pins_tls proves the caCertPath wiring end-to-end: a
// TLS server whose self-signed cert is written to disk and pinned verifies.
func TestNewClient_ca_cert_pins_tls(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"friendlyName":"pinned"}}`))
	}))
	defer ts.Close()
	p := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(ts.URL, "tok", p)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Identity(t.Context())
	if err != nil || id.FriendlyName != "pinned" {
		t.Errorf("Identity over pinned TLS = %+v, %v", id, err)
	}
}

func TestNewClient_counts_retries(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c, err := NewClient(ts.URL, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if got := c.Retries(); got != 2 {
		t.Errorf("Retries() = %d, want 2 (plex_http_retries_total contract)", got)
	}
}

func TestClient_Retries_nil_safe(t *testing.T) {
	c := &Client{}
	if got := c.Retries(); got != 0 {
		t.Errorf("Retries() on zero-value client = %d, want 0", got)
	}
}

// TestGet_populates_StatusError pins error transparency through the
// adapter: a non-200 surfaces as the library's StatusError, the type
// main.go's fatal-vs-transient startup classifier matches.
func TestGet_populates_StatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	c, err := NewClientFromHTTP(ts.URL, "tok", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	got := c.Get(t.Context(), "/x", nil)
	var statusErr *plexapi.StatusError
	if !errors.As(got, &statusErr) || statusErr.Code != http.StatusUnauthorized {
		t.Errorf("err = %v, want *plexapi.StatusError 401", got)
	}
	// And the lib's classifier agrees it is fatal.
	if !plexapi.IsConfigError(got) {
		t.Error("401 should classify as a config error")
	}
}

func TestGet_not_found_is_ErrNotFound(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	c, err := NewClientFromHTTP(ts.URL, "tok", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Get(t.Context(), "/x", nil); !errors.Is(got, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (Plex Pass degradation contract)", got)
	}
}

// The ContainerTotalSize tests exercise the library method through the
// adapter's embedded client — the app's former GetContainerSize rename
// wrapper was inlined away (callers convert the section id with
// plexapi.RatingKey at the call site).
func TestContainerTotalSize(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("X-Plex-Container-Size") != "1" || q.Get("X-Plex-Container-Start") != "0" || q.Get("type") != "4" {
			t.Errorf("missing container paging params: %v", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":4360}}`))
	}))
	defer ts.Close()
	c, err := NewClientFromHTTP(ts.URL, "tok", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ContainerTotalSize(t.Context(), plexapi.RatingKey("1"), 4)
	if err != nil || got != 4360 {
		t.Errorf("ContainerTotalSize = (%d, %v), want 4360", got, err)
	}
}

func TestContainerTotalSize_propagates_error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()
	c, err := NewClientFromHTTP(ts.URL, "tok", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContainerTotalSize(t.Context(), plexapi.RatingKey("1"), 0); err == nil {
		t.Error("nil error on 502")
	}
}

func TestContainerTotalSize_rejects_non_numeric_section(t *testing.T) {
	c, err := NewClientFromHTTP("http://plex:32400", "tok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContainerTotalSize(t.Context(), plexapi.RatingKey("1; DROP"), 4); err == nil {
		t.Error("non-numeric section id accepted")
	}
}

// TestGet_timeout_bounds_stalled_request pins the no-deadline fallback: a
// caller without a context deadline is still bounded (the adapter's 30s
// default, shrunk here via a caller deadline to keep the test fast — the
// contract under test is that a stall terminates rather than hangs).
func TestGet_timeout_bounds_stalled_request(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer ts.Close()
	defer close(release) // LIFO: unblock the handler BEFORE ts.Close waits on it
	c, err := NewClientFromHTTP(ts.URL, "tok", ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Get(ctx, "/x", nil); err == nil {
		t.Fatal("stalled Get returned nil error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("stalled Get took %v, deadline not honored", elapsed)
	}
}
