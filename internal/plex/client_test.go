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

	"github.com/cplieger/httpx"
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

// newRetryTestClient wires a test Client whose transport is wrapped in an
// httpx retry round-tripper with a microsecond base delay (for test speed)
// and the given retry count (maxRetries == retries after the initial
// attempt, so total attempts == maxRetries+1). This mirrors what NewClient
// installs in production, letting the retry tests exercise the real httpx
// path now that GetWithRetry no longer owns a per-call retry loop.
func newRetryTestClient(t *testing.T, ts *httptest.Server, maxRetries int) *Client {
	t.Helper()
	return newRetryTestClientDelay(t, ts, maxRetries, time.Microsecond)
}

// newRetryTestClientDelay is newRetryTestClient with an explicit backoff base
// delay, used by tests that need a real backoff window (e.g. cancel-during-
// backoff) rather than the microsecond default.
func newRetryTestClientDelay(t *testing.T, ts *httptest.Server, maxRetries int, base time.Duration) *Client {
	t.Helper()
	c := NewTestClientFromServer(t, ts)
	c.HTTPClient.Transport = httpx.NewRetryRoundTripper(
		c.HTTPClient.Transport,
		httpx.WithMaxRetries(maxRetries),
		httpx.WithRTBaseDelay(base),
	)
	return c
}

// Retry tests below exercise the httpx retry transport that NewClient now
// installs (see newRetryTestClient). The retried set is httpx's default —
// transient transport errors + 429/502/503/504 — so these tests use 503 as
// the retryable fixture (the prior hand-rolled loop retried 500 too; httpx
// deliberately does not). The final GetWithRetry int arg is ignored now that
// the transport owns the attempt count.

func TestGetWithRetry_zero_retries_makes_single_attempt(t *testing.T) {
	// maxRetries=0 on the transport => exactly 1 attempt, even for a
	// retryable status. Also proves the GetWithRetry int arg is ignored:
	// we pass 99 yet still get a single attempt.
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newRetryTestClient(t, ts, 0)
	var resp plexapi.MC[plexapi.ServerIdentity]
	_ = client.GetWithRetry(context.Background(), "/", &resp, 99)

	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (0 retries; call arg is ignored)", got)
	}
}

func TestGetWithRetry_makes_maxRetries_plus_one_attempts(t *testing.T) {
	// maxRetries=3 on the transport => 4 total attempts on a persistently
	// retryable (503) response.
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newRetryTestClient(t, ts, 3)
	var resp plexapi.MC[plexapi.ServerIdentity]
	_ = client.GetWithRetry(context.Background(), "/", &resp, 0)

	if got := attempts.Load(); got != 4 {
		t.Errorf("attempts = %d, want 4 (maxRetries=3 => 4 attempts)", got)
	}
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

func TestGetWithRetry(t *testing.T) {
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"RetryPlex"}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// maxRetries=2 => up to 3 attempts; 503,503,200 succeeds on the 3rd.
	client := newRetryTestClient(t, ts, 2)
	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(context.Background(), "/", &resp, 0)
	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if resp.MediaContainer.FriendlyName != "RetryPlex" {
		t.Errorf("name = %q, want RetryPlex", resp.MediaContainer.FriendlyName)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestGetWithRetryExhausted(t *testing.T) {
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// maxRetries=1 => 2 attempts, then the last 503 surfaces unchanged.
	client := newRetryTestClient(t, ts, 1)
	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(context.Background(), "/", &resp, 0)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// Caller-side mapping is preserved: a non-200 becomes *HTTPStatusError.
	se, ok := errors.AsType[*HTTPStatusError](err)
	if !ok {
		t.Fatalf("err = %T, want *HTTPStatusError", err)
	}
	if se.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want 503", se.Code)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (maxRetries=1 => 2 attempts)", got)
	}
}

func TestGetWithRetryCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newRetryTestClient(t, ts, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(ctx, "/", &resp, 0)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// TestGetWithRetry_honors_retry_after_on_429 is the defect-fix test. The
// prior hand-rolled loop IGNORED the Retry-After header on 429 and used its
// own exponential backoff; the httpx retry transport honors it. With a
// microsecond base delay, a Retry-After of 1s forces the retry to wait ~1s
// (not microseconds), proving the header is respected.
func TestGetWithRetry_honors_retry_after_on_429(t *testing.T) {
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"OK"}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newRetryTestClient(t, ts, 2)
	var resp plexapi.MC[plexapi.ServerIdentity]
	start := time.Now()
	if err := client.GetWithRetry(context.Background(), "/", &resp, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(start)

	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (429 then 200)", got)
	}
	// Retry-After=1 honored: with a microsecond base delay, the only way the
	// retry waits ~1s is by honoring the header. Allow scheduler slack.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~1s (Retry-After must be honored, not the microsecond base delay)", elapsed)
	}
}

func TestGetWithRetry_no_retry_on_4xx(t *testing.T) {
	// With retries enabled (maxRetries=3), 4xx responses other than 429 must
	// still make exactly 1 attempt — httpx's default retry set excludes them.
	for _, code := range []int{400, 401, 403, 405, 422} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			var attempts atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(code)
			})
			ts := httptest.NewServer(handler)
			defer ts.Close()

			client := newRetryTestClient(t, ts, 3)
			var resp plexapi.MC[plexapi.ServerIdentity]
			_ = client.GetWithRetry(context.Background(), "/", &resp, 0)

			if got := attempts.Load(); got != 1 {
				t.Errorf("status %d: attempts = %d, want 1 (no retry on 4xx)", code, got)
			}
		})
	}
}

func TestGetWithRetry_retries_on_429(t *testing.T) {
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"OK"}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newRetryTestClient(t, ts, 2)
	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(context.Background(), "/", &resp, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (429 should be retried)", got)
	}
}

func TestGetWithRetry_cancellation_during_backoff(t *testing.T) {
	// Cancelling the context during the backoff delay between retries must
	// abort promptly with a non-nil error and not exhaust all attempts
	// (httpx's SleepCtx returns the context error).
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 10s base delay guarantees a wide backoff window to cancel within.
	client := newRetryTestClientDelay(t, ts, 5, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay — long enough for the first attempt to
	// complete and enter the backoff wait, but before the 10s wait elapses.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(ctx, "/", &resp, 0)
	if err == nil {
		t.Fatal("expected error when context cancelled during backoff")
	}
	got := attempts.Load()
	if got < 1 {
		t.Errorf("attempts = %d, want >= 1", got)
	}
	// Should NOT have exhausted all 6 attempts (cancelled during backoff).
	if got >= 6 {
		t.Errorf("attempts = %d, want < 6 (should cancel during backoff)", got)
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
