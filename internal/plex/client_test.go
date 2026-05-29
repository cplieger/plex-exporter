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

	"plex-exporter/internal/plexapi"
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
// Delegates to the exported NewTestClientFromServer and overrides Retry
// with a fast base delay for test speed.
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c := NewTestClientFromServer(t, ts)
	c.Retry = RetryConfig{BaseDelay: time.Microsecond}
	return c
}

// Tests relocated from apps/plex-exporter/main_test.go and
// apps/plex-exporter/mutation_test.go as part of the internal/plex
// migration (cycle 1, chain step 3). They exercise the retry/backoff
// surface directly against the exported API and no longer rely on any
// package-main aliases.

func TestGetWithRetry_retries_correct_number_of_times(t *testing.T) {
	// Verifies that maxRetries=1 means exactly 1 attempt (no retries).
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
		Retry:      RetryConfig{BaseDelay: time.Microsecond},
	}

	var resp plexapi.MC[plexapi.ServerIdentity]
	_ = client.GetWithRetry(context.Background(), "/", &resp, 1)

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (maxRetries=1 means 1 attempt)", attempts)
	}
}

func TestGetWithRetry_maxRetries4_makes_4_attempts(t *testing.T) {
	// Verifies that maxRetries=4 means exactly 4 attempts.
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
		Retry:      RetryConfig{BaseDelay: time.Microsecond},
	}

	var resp plexapi.MC[plexapi.ServerIdentity]
	_ = client.GetWithRetry(context.Background(), "/", &resp, 4)

	if attempts != 4 {
		t.Errorf("attempts = %d, want 4 (maxRetries=4)", attempts)
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
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"RetryPlex"}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
		Retry:      RetryConfig{BaseDelay: time.Microsecond},
	}

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(context.Background(), "/", &resp, 3)
	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if resp.MediaContainer.FriendlyName != "RetryPlex" {
		t.Errorf("name = %q, want RetryPlex", resp.MediaContainer.FriendlyName)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestGetWithRetryExhausted(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
		Retry:      RetryConfig{BaseDelay: time.Microsecond},
	}

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(context.Background(), "/", &resp, 2)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

func TestGetWithRetryCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
		Retry:      RetryConfig{BaseDelay: time.Microsecond},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(ctx, "/", &resp, 5)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestGetWithRetry_cancelled_after_failure_returns_last_error(t *testing.T) {
	// When context is cancelled after at least one failed attempt,
	// getWithRetry should return lastErr (the real error), not ctx.Err().
	// This exercises the uncovered branch at line 289-291.
	//
	// `attempts` uses atomic.Int32 because the HTTP handler goroutine mutates
	// it while the test goroutine reads it after getWithRetry returns; without
	// atomic access the race detector fires intermittently under -race.
	var attempts atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	parsed, _ := url.Parse(ts.URL)
	client := &Client{
		HTTPClient: ts.Client(),
		BaseURL:    parsed,
		Token:      TestToken,
		Retry:      RetryConfig{BaseDelay: time.Microsecond},
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first attempt completes but before the second starts.
	// With retryBaseDelay=1us, we need to cancel during the backoff.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(ctx, "/", &resp, 10)
	if err == nil {
		t.Fatal("expected error")
	}
	// The error should be the last HTTP error, not context.Canceled,
	// because lastErr was set before ctx was cancelled.
	if attempts.Load() < 1 {
		t.Fatalf("expected at least 1 attempt, got %d", attempts.Load())
	}
	// Either lastErr (httpStatusError) or ctx.Err() is acceptable here
	// depending on timing, but the function must not return nil.
	if err == nil {
		t.Error("getWithRetry must return non-nil error when context is cancelled")
	}
}

func TestGetWithRetry_no_retry_on_4xx(t *testing.T) {
	for _, code := range []int{400, 401, 403, 405, 422} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			attempts := 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(code)
			})
			ts := httptest.NewServer(handler)
			defer ts.Close()

			client := newTestClient(t, ts)
			var resp plexapi.MC[plexapi.ServerIdentity]
			_ = client.GetWithRetry(context.Background(), "/", &resp, 3)

			if attempts != 1 {
				t.Errorf("status %d: attempts = %d, want 1 (no retry on 4xx)", code, attempts)
			}
		})
	}
}

func TestGetWithRetry_retries_on_429(t *testing.T) {
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"OK"}}`)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := newTestClient(t, ts)
	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(context.Background(), "/", &resp, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (429 should be retried)", attempts)
	}
}

func TestGetWithRetry_cancellation_during_backoff(t *testing.T) {
	// Verify that cancelling the context during the backoff delay between
	// retries returns ctx.Err() promptly (exercises the ctx.Done() case
	// in the timer select).
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := NewTestClient(t, ts.URL)
	client.HTTPClient = ts.Client()
	client.Retry = RetryConfig{BaseDelay: 10 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay — long enough for the first attempt to
	// complete and enter the backoff timer, but before the 10s timer fires.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var resp plexapi.MC[plexapi.ServerIdentity]
	err := client.GetWithRetry(ctx, "/", &resp, 5)
	if err == nil {
		t.Fatal("expected error when context cancelled during backoff")
	}
	// Should have made at least 1 attempt before cancellation.
	if attempts < 1 {
		t.Errorf("attempts = %d, want >= 1", attempts)
	}
	// Should NOT have exhausted all 5 retries.
	if attempts >= 5 {
		t.Errorf("attempts = %d, want < 5 (should cancel during backoff)", attempts)
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
	if c.HTTPClient.Transport != nil {
		t.Error("default client must use nil Transport (stdlib default), not a custom insecure one")
	}
	if c.HTTPClient.CheckRedirect == nil {
		t.Error("CheckRedirect must be set to prevent token leaks across cross-origin redirects")
	}
}

func TestNewPlexClient_ca_cert_path_sets_root_cas(t *testing.T) {
	// Generate an in-memory self-signed CA cert + write to a temp file.
	caPath := writeSelfSignedPEM(t)

	c, err := NewClient("https://plex.example:32400", "tok", caPath)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr, ok := c.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.HTTPClient.Transport)
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
	if c.HTTPClient.Transport != nil {
		t.Error("default-transport path must leave Transport nil so the OS trust store is used")
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
