package plex

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/plex-exporter/internal/plexapi"
)

// writeSelfSignedPEM generates an in-memory self-signed CA cert and writes
// it as PEM to a tempfile under t.TempDir(). Returns the path. Used by the
// CA-pool tests; the cert is never actually validated against, just parsed.
func writeSelfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-plex-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

// newTestClient wires a test Client against the given httptest.Server.
// Delegates to the exported NewTestClientFromServer. The returned client has
// no retry transport, so each Get is a single attempt — the right default
// for the non-retry tests below.
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	return NewTestClientFromServer(t, ts)
}

func TestHTTPStatusError_Error(t *testing.T) {
	// Structured formatting for error messages on non-200/non-404
	// responses. Kept distinct from a bare error so callers can classify
	// 4xx vs 5xx via errors.As / errors.AsType.
	e := &HTTPStatusError{Code: 500, Status: "500 Internal Server Error", Path: "/test/path"}
	got := e.Error()
	want := "plex API /test/path: 500 Internal Server Error"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestGetWithHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != TestToken {
			t.Error("missing plex token header")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing accept header")
		}
		switch r.URL.Path {
		case "/test":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"TestPlex"}}`)
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
	}

	t.Run("success", func(t *testing.T) {
		var resp plexapi.MC[plexapi.ServerIdentity]
		err := client.Get(context.Background(), "/test", &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.MediaContainer.FriendlyName != "TestPlex" {
			t.Errorf("name = %q, want TestPlex", resp.MediaContainer.FriendlyName)
		}
	})

	t.Run("not found", func(t *testing.T) {
		var resp plexapi.MC[plexapi.ServerIdentity]
		err := client.Get(context.Background(), "/notfound", &resp)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		var resp plexapi.MC[plexapi.ServerIdentity]
		err := client.Get(context.Background(), "/error", &resp)
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if errors.Is(err, ErrNotFound) {
			t.Error("should not be ErrNotFound")
		}
	})

	t.Run("extra headers", func(t *testing.T) {
		var resp plexapi.MC[plexapi.ServerIdentity]
		err := client.GetWithHeaders(context.Background(), "/test", &resp, map[string]string{
			"X-Custom": "value",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestGetWithHeaders_invalid_url_returns_error(t *testing.T) {
	// Targets uncovered line 191-193: url.Parse error path.
	parsed, _ := url.Parse("http://localhost")
	client := &Client{
		HTTPClient: &http.Client{},
		BaseURL:    parsed,
		Token:      TestToken,
	}

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithHeaders(context.Background(), "://invalid", &resp, nil)
	if err == nil {
		t.Fatal("expected error for invalid URL path")
	}
}

func TestGetWithHeaders_invalid_json_returns_error(t *testing.T) {
	// Targets the json.Unmarshal error path at the end of getWithHeaders.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `not json at all`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newTestClient(t, ts)

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.Get(context.Background(), "/test", &resp)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestGetWithHeaders_empty_body_returns_nil(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// No body written — Content-Length is 0.
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newTestClient(t, ts)

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.Get(context.Background(), "/empty", &resp)
	if err != nil {
		t.Fatalf("getWithHeaders(empty body) unexpected error: %v", err)
	}
	// resp should be zero-value — no unmarshal attempted.
	if resp.MediaContainer.FriendlyName != "" {
		t.Errorf("FriendlyName = %q, want empty (no body to unmarshal)", resp.MediaContainer.FriendlyName)
	}
}

func TestGetContainerSize(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Container-Start") != "0" {
			t.Error("missing container start header")
		}
		if r.Header.Get("X-Plex-Container-Size") != "1" {
			t.Error("missing container size header")
		}
		fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
	}

	size, err := client.GetContainerSize(context.Background(), "/library/sections/1/all")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 42 {
		t.Errorf("size = %d, want 42", size)
	}
}

func TestNewPlexClient_default_no_tls_skip(t *testing.T) {
	c, err := NewClient("http://plex.example:32400", "tok", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.BaseURL.String() != "http://plex.example:32400" {
		t.Errorf("baseURL = %q, want http://plex.example:32400", c.BaseURL.String())
	}
	if c.Token != "tok" {
		t.Errorf("token = %q, want tok", c.Token)
	}
	if c.HTTPClient.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", c.HTTPClient.Timeout)
	}
	if _, ok := c.HTTPClient.Transport.(*httpx.RetryRoundTripper); !ok {
		t.Errorf("Transport = %T, want *httpx.RetryRoundTripper (retry transport installed by NewClient)", c.HTTPClient.Transport)
	}
	if c.HTTPClient.CheckRedirect == nil {
		t.Error("CheckRedirect must be set to prevent token leaks across cross-origin redirects")
	}
}

func TestNewPlexClient_ca_cert_path_sets_root_cas(t *testing.T) {
	// Generate an in-memory self-signed CA cert + write to a temp file.
	caPath := writeSelfSignedPEM(t)

	// NewClient wraps the CA-pinned transport in the retry round-tripper.
	c, err := NewClient("https://plex.example:32400", "tok", caPath)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, ok := c.HTTPClient.Transport.(*httpx.RetryRoundTripper); !ok {
		t.Errorf("Transport = %T, want *httpx.RetryRoundTripper wrapping the CA-pinned transport", c.HTTPClient.Transport)
	}

	// The retry round-tripper hides its inner transport, so verify the CA
	// pinning on the transport plexTLSTransport builds (the same value
	// NewClient wraps): RootCAs populated, TLS 1.2 min, verification on.
	tr, err := plexTLSTransport(caPath)
	if err != nil {
		t.Fatalf("plexTLSTransport: %v", err)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig must be set when caCertPath is provided")
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Error("RootCAs must be populated from caCertPath")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must remain false; caCertPath is the SECURE path")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want tls.VersionTLS12 (%d)",
			tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
}

func TestNewPlexClient_no_ca_cert_uses_default_transport(t *testing.T) {
	c, err := NewClient("https://plex.example:32400", "tok", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// With no CA cert, NewClient still installs the retry round-tripper but
	// wraps a nil base, so httpx falls back to http.DefaultTransport (OS
	// trust store). No custom/insecure *http.Transport is created.
	if _, ok := c.HTTPClient.Transport.(*httpx.RetryRoundTripper); !ok {
		t.Errorf("Transport = %T, want *httpx.RetryRoundTripper over the default transport", c.HTTPClient.Transport)
	}
}

func TestNewPlexClient_ca_cert_path_missing_file_errors(t *testing.T) {
	_, err := NewClient("https://plex.example:32400", "tok", "/nonexistent/ca.pem")
	if err == nil {
		t.Fatal("NewClient should error when caCertPath points to a missing file")
	}
	if !strings.Contains(err.Error(), "PLEX_CA_CERT_PATH") {
		t.Errorf("error should mention PLEX_CA_CERT_PATH for diagnosability; got %v", err)
	}
}

func TestNewPlexClient_ca_cert_path_invalid_pem_errors(t *testing.T) {
	tmp := t.TempDir()
	bogus := filepath.Join(tmp, "bogus.pem")
	if err := os.WriteFile(bogus, []byte("not a pem file"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := NewClient("https://plex.example:32400", "tok", bogus)
	if err == nil {
		t.Fatal("NewClient should error when caCertPath has no PEM-encoded certs")
	}
}

func TestNewPlexClient_refuses_redirects(t *testing.T) {
	// Verify the CheckRedirect policy: the Plex API doesn't redirect, so
	// any 302 is suspicious (potential token-exfiltration via attacker-
	// controlled upstream). Go's default CheckRedirect would follow up to
	// 10 redirects; ours must refuse the first one.
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{URL: &url.URL{Scheme: "http", Host: "evil.example"}}, "http://evil.example/", http.StatusFound)
	}))
	defer redirectSrv.Close()

	c, err := NewClient(redirectSrv.URL, "tok", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// A redirect response should surface to the caller as a non-2xx status
	// (not be silently followed). With CheckRedirect: ErrUseLastResponse,
	// the transport returns the 302 as-is.
	resp, err := c.HTTPClient.Get(redirectSrv.URL + "/")
	if err != nil {
		t.Fatalf("expected no error from ErrUseLastResponse path, got %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d (redirect not followed)", resp.StatusCode, http.StatusFound)
	}
}

func TestNewPlexClient_invalid_url_returns_error(t *testing.T) {
	_, err := NewClient("://missing-scheme", "tok", "")
	if err == nil {
		t.Fatal("NewClient with invalid URL should return error")
	}
}

// TestNewClient_counts_retries verifies the WithOnRetry hook installed by
// NewClient increments the client's retry counter (surfaced as the
// plex_http_retries_total metric). The server returns one 503 (retried by the
// round-tripper) then 200, so exactly one retry is recorded.
func TestNewClient_counts_retries(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // retried by the round-tripper
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"friendlyName":"TestPlex"}}`))
	}))
	defer ts.Close()

	c, err := NewClient(ts.URL, "tok", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.Retries(); got != 0 {
		t.Fatalf("Retries() before any request = %d, want 0", got)
	}

	var resp plexapi.MC[plexapi.ServerIdentity]
	if err := c.Get(context.Background(), "/", &resp); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.MediaContainer.FriendlyName != "TestPlex" {
		t.Errorf("friendlyName = %q, want TestPlex", resp.MediaContainer.FriendlyName)
	}
	if got := c.Retries(); got != 1 {
		t.Errorf("Retries() = %d, want 1 (one 503 retried to 200)", got)
	}
}

// TestClient_Retries_nil_safe confirms a Client built without NewClient (no
// retry hook installed, e.g. a test fixture) reports zero retries rather than
// panicking on the nil counter.
func TestClient_Retries_nil_safe(t *testing.T) {
	var c Client
	if got := c.Retries(); got != 0 {
		t.Errorf("Retries() on zero-value Client = %d, want 0", got)
	}
}
