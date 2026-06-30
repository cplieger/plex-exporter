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

// testToken is the fixed credential used by the test fixtures in this package.
// The leading "$" mimics an unexpanded environment-variable placeholder, which
// the repo secret-scan regex deliberately excludes.
const testToken = "$fixture-test-token"

// newTestClient wires a test Client against the given httptest.Server. The
// returned client has no retry transport, so each Get is a single attempt —
// the right default for the non-retry tests below.
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{BaseURL: u, Token: testToken, HTTPClient: ts.Client()}
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

// TestGet_populates_HTTPStatusError pins the documented contract that Get
// returns a *HTTPStatusError carrying the real response Code/Status/Path so
// callers can classify 4xx (no retry) vs 5xx (retry) via errors.As. The
// existing TestHTTPStatusError_Error only checks Error() on a hand-built
// value and the TestGetWithHeaders "server error" subtest only checks the
// error is non-nil and not ErrNotFound, so a mutation of the Code/Status/Path
// fields populated in Get survives.
func TestGet_populates_HTTPStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := newTestClient(t, ts)

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.Get(context.Background(), "/error", &resp)
	var se *HTTPStatusError
	if !errors.As(err, &se) {
		t.Fatalf("Get on 500 = %v, want *HTTPStatusError", err)
	}
	if se.Code != http.StatusInternalServerError {
		t.Errorf("HTTPStatusError.Code = %d, want %d", se.Code, http.StatusInternalServerError)
	}
	if se.Path != "/error" {
		t.Errorf("HTTPStatusError.Path = %q, want %q", se.Path, "/error")
	}
}

func TestGetWithHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != testToken {
			t.Error("missing plex token header")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing accept header")
		}
		switch r.URL.Path {
		case "/test":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"MediaContainer":{"platform":"TestPlex"}}`)
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
		Token:      testToken,
	}

	t.Run("success", func(t *testing.T) {
		var resp plexapi.MC[plexapi.ServerIdentity]
		err := client.Get(context.Background(), "/test", &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.MediaContainer.Platform != "TestPlex" {
			t.Errorf("platform = %q, want TestPlex", resp.MediaContainer.Platform)
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
	// The url.Parse error path: a malformed request path must surface as an
	// error rather than being silently dropped.
	parsed, _ := url.Parse("http://localhost")
	client := &Client{
		HTTPClient: &http.Client{},
		BaseURL:    parsed,
		Token:      testToken,
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
	if resp.MediaContainer.Platform != "" {
		t.Errorf("Platform = %q, want empty (no body to unmarshal)", resp.MediaContainer.Platform)
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
		Token:      testToken,
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
		_, _ = w.Write([]byte(`{"MediaContainer":{"platform":"TestPlex"}}`))
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
	if resp.MediaContainer.Platform != "TestPlex" {
		t.Errorf("platform = %q, want TestPlex", resp.MediaContainer.Platform)
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

func TestGetContainerSize_propagates_error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := newTestClient(t, ts)

	size, err := client.GetContainerSize(context.Background(), "/library/sections/1/all")
	if err == nil {
		t.Fatal("GetContainerSize on a 500 response = nil error, want the error propagated (a swallowed error would silently report 0 items)")
	}
	if size != 0 {
		t.Errorf("size = %d, want 0 on error", size)
	}
}

func TestNewClient_rejects_invalid_server_url(t *testing.T) {
	tests := []struct {
		name      string
		serverURL string
	}{
		{name: "non-http scheme", serverURL: "ftp://plex.example:32400"},
		{name: "file scheme", serverURL: "file:///etc/passwd"},
		{name: "empty host", serverURL: "http://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewClient(tt.serverURL, "tok", ""); err == nil {
				t.Errorf("NewClient(%q) = nil error, want a validation error", tt.serverURL)
			}
		})
	}
}

func TestGetWithHeaders_rejects_absolute_path(t *testing.T) {
	// An absolute or scheme-relative reference would let ResolveReference override the configured
	// BaseURL host and leak X-Plex-Token to an attacker-controlled origin, so every such path must
	// be rejected before a request is built. No httptest server is needed: the guard returns before
	// HTTPClient.Do.
	base, err := url.Parse("http://plex.example:32400")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	client := &Client{HTTPClient: &http.Client{}, BaseURL: base, Token: testToken}

	paths := []struct {
		name string
		path string
	}{
		{"absolute http url", "http://evil.example/library"},
		{"absolute https url", "https://evil.example/library"},
		{"scheme-relative url", "//evil.example/library"},
	}
	for _, tt := range paths {
		t.Run(tt.name, func(t *testing.T) {
			var resp plexapi.MC[plexapi.ServerIdentity]
			err := client.GetWithHeaders(context.Background(), tt.path, &resp, nil)
			if err == nil {
				t.Fatalf("GetWithHeaders(%q) = nil error, want rejection of an off-host path (X-Plex-Token leak guard)", tt.path)
			}
			if !strings.Contains(err.Error(), "must be relative to the configured server") {
				t.Errorf("GetWithHeaders(%q) error = %v, want the relative-path rejection", tt.path, err)
			}
		})
	}
}

// TestGet_enforces_response_body_size_cap pins the 10 MB MaxResponseBody OOM
// guard at its boundary: a body of exactly MaxResponseBody bytes still decodes
// (the cap is inclusive), while one byte past it is rejected with a size-limit
// error before any decode. Guards against a compromised or buggy Plex returning
// an unbounded body.
func TestGet_enforces_response_body_size_cap(t *testing.T) {
	const pre = `{"MediaContainer":{"platform":"`
	const post = `"}}`
	payloadLen := MaxResponseBody - len(pre) - len(post)

	tests := []struct {
		name       string
		bodyLen    int
		wantErr    bool
		wantErrSub string
		wantPlat   int
	}{
		{name: "exactly at cap decodes", bodyLen: payloadLen, wantErr: false, wantPlat: payloadLen},
		{name: "one byte over cap rejected", bodyLen: payloadLen + 1, wantErr: true, wantErrSub: "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := pre + strings.Repeat("x", tt.bodyLen) + post
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, body)
			}))
			defer ts.Close()

			client := newTestClient(t, ts)
			var resp plexapi.MC[plexapi.ServerIdentity]
			err := client.Get(context.Background(), "/identity", &resp)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Get with a %d-byte body = nil error, want a size-limit error", len(body))
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("Get error = %v, want it to contain %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get with a %d-byte body (== cap) = %v, want success (cap is inclusive)", len(body), err)
			}
			if got := len(resp.MediaContainer.Platform); got != tt.wantPlat {
				t.Errorf("decoded Platform length = %d, want %d", got, tt.wantPlat)
			}
		})
	}
}

// TestNewClient_records_retry_on_transport_error exercises the WithOnRetry
// hook on the transport-error path, where the retry round-tripper invokes the
// hook with a nil *http.Response because no HTTP reply was ever received. A
// dial to a closed listener is refused, which the transport classifies as a
// transient error and retries, so the retry counter advances. This complements
// TestNewClient_counts_retries, which only drives the 503 path (non-nil resp).
func TestNewClient_records_retry_on_transport_error(t *testing.T) {
	// Stand up a server only to obtain a real loopback address, then close it
	// so every subsequent dial to that port is refused (a transient error).
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := ts.URL
	ts.Close()

	c, err := NewClient(closedURL, "tok", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var resp plexapi.MC[plexapi.ServerIdentity]
	err = c.Get(ctx, "/", &resp)
	if err == nil {
		t.Fatal("Get against a closed listener = nil error, want a transport error after retries are exhausted")
	}
	if got := c.Retries(); got == 0 {
		t.Errorf("Retries() = 0, want > 0: a transient transport error must be retried and counted")
	}
}
