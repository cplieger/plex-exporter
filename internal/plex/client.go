package plex

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cplieger/httpx"
	"github.com/cplieger/plex-exporter/internal/plexapi"
)

// MaxResponseBody caps the bytes we read from a Plex HTTP response to
// prevent OOM on unexpected payloads. 10 MiB covers the largest realistic
// /status/sessions and /library responses with headroom.
const MaxResponseBody = 10 << 20 // 10 MB

// Retry transport defaults installed by NewClient. defaultMaxRetries is the
// number of retries *after* the initial attempt, so 2 == 3 total attempts —
// matching the prior GetWithRetry(maxRetries=3) production call sites.
// defaultRetryBaseDelay matches the prior hand-rolled retryBaseDelay default.
const (
	defaultMaxRetries     = 2
	defaultRetryBaseDelay = 100 * time.Millisecond
)

// ErrNotFound is returned by Get/GetWithHeaders when the Plex server
// responds 404. Callers can classify it via errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("not found")

// HTTPStatusError is returned by Get for non-200, non-404 responses. Kept
// distinct from a bare error so callers can classify 4xx (do not retry)
// vs 5xx (retry) via errors.As / errors.AsType.
type HTTPStatusError struct {
	Status string
	Path   string
	Code   int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("plex API %s: %s", e.Path, e.Status)
}

// Client is the Plex HTTP client. Fields are exported so the internal
// composition root (package main, package server, package wsclient) can
// construct test fixtures and read configuration without accessor noise;
// the whole internal/* tree is a single trust boundary.
type Client struct {
	HTTPClient *http.Client
	BaseURL    *url.URL
	Token      string
}

// NewClient parses serverURL and returns a Client configured with a safe
// default HTTP transport. When caCertPath is non-empty, the PEM file at
// that path is loaded into the TLS RootCAs pool — TLS verification stays
// ENABLED, pinned to that CA. This is the recommended setup for users
// running Plex with a self-signed certificate.
//
// The transport is wrapped in an httpx retry round-tripper that retries
// 429/502/503/504 responses and transient transport errors with jittered
// exponential backoff, HONORING the Retry-After header on 429. Retry count
// and base delay are fixed at construction (see defaultMaxRetries /
// defaultRetryBaseDelay); every Get on the returned Client benefits from
// this without a per-call retry loop.
//
// When caCertPath is empty:
//   - For "https://hash.plex.direct:32400" URLs, Plex's public Let's
//     Encrypt cert validates against the OS trust store. No env var needed.
//   - For "https://<self-signed-host>:32400" URLs, the connection will
//     FAIL on cert verification. Set PLEX_CA_CERT_PATH to the server's
//     CA cert.
//   - For "http://..." URLs, TLS isn't in play; this transport config is
//     a no-op.
func NewClient(serverURL, token, caCertPath string) (*Client, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parsing PLEX_SERVER URL: %w", err)
	}

	// base stays a nil http.RoundTripper interface when no CA cert is
	// configured, so the retry round-tripper falls back to
	// http.DefaultTransport (OS trust store). Only assign when caCertPath
	// is set: storing a typed-nil *http.Transport in the interface would
	// defeat NewRetryRoundTripper's nil check and panic on RoundTrip.
	var base http.RoundTripper
	if caCertPath != "" {
		tr, tErr := plexTLSTransport(caCertPath)
		if tErr != nil {
			return nil, tErr
		}
		base = tr
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		// Retry transport wraps the (CA-pinned or default) base transport.
		// It honors Retry-After on 429 and retries 429/502/503/504 +
		// transient transport errors with jittered exponential backoff.
		Transport: httpx.NewRetryRoundTripper(base,
			httpx.WithMaxRetries(defaultMaxRetries),
			httpx.WithRTBaseDelay(defaultRetryBaseDelay),
		),
		// Plex's API does not issue redirects; refuse to follow any. Go's
		// default CheckRedirect forwards custom headers (including
		// X-Plex-Token) on cross-origin redirects, which would leak the
		// token to an attacker-controlled host if Plex ever served a 302.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{BaseURL: parsed, Token: token, HTTPClient: httpClient}, nil
}

// plexTLSTransport builds a CA-pinned *http.Transport from caCertPath. TLS
// verification stays ENABLED (RootCAs pinned, TLS 1.2 minimum,
// InsecureSkipVerify false). Returns an error for an unreadable file or a
// PEM that contains no certificates. caCertPath must be non-empty.
func plexTLSTransport(caCertPath string) (*http.Transport, error) {
	pemBytes, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("reading PLEX_CA_CERT_PATH=%q: %w", caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("PLEX_CA_CERT_PATH=%q: no PEM-encoded certificates found", caCertPath)
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}, nil
}

// Get fetches path and unmarshals the JSON body into result. Returns
// ErrNotFound for 404, *HTTPStatusError for other non-2xx. When the Client
// was built by NewClient, transient failures (429/502/503/504 + transport
// errors) are retried transparently by the retry transport, honoring
// Retry-After on 429.
func (c *Client) Get(ctx context.Context, path string, result any) error {
	return c.GetWithHeaders(ctx, path, result, nil)
}

// GetWithHeaders is Get with additional request headers merged on top of
// the defaults (Accept, X-Plex-Token).
func (c *Client) GetWithHeaders(ctx context.Context, path string, result any, extra map[string]string) error {
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL.ResolveReference(ref).String(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPStatusError{Code: resp.StatusCode, Status: resp.Status, Path: path}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, result)
}

// GetContainerSize fetches a library section with one item and reads the
// totalSize field from the JSON body. This is more reliable than the
// X-Plex-Container-Total-Size header, which doesn't work for type-filtered
// queries (e.g. ?type=4 for episodes).
func (c *Client) GetContainerSize(ctx context.Context, path string) (int64, error) {
	var resp plexapi.MC[struct {
		TotalSize int64 `json:"totalSize"`
	}]
	if err := c.GetWithHeaders(ctx, path, &resp, map[string]string{
		"X-Plex-Container-Start": "0",
		"X-Plex-Container-Size":  "1",
	}); err != nil {
		return 0, err
	}
	return resp.MediaContainer.TotalSize, nil
}

// GetWithRetry fetches path with automatic retry. Retry behavior — attempt
// count, jittered exponential backoff, Retry-After honoring on 429, and the
// retried set (429/502/503/504 + transient transport errors) — is now
// provided by the client's retry transport, configured in NewClient. The
// final int argument is retained for call-site compatibility and no longer
// controls the attempt count (the retry transport owns it). 404 maps to
// ErrNotFound; other non-200 responses map to *HTTPStatusError; the 10 MiB
// body cap and JSON decoding are preserved via Get/GetWithHeaders.
func (c *Client) GetWithRetry(ctx context.Context, path string, result any, _ int) error {
	return c.Get(ctx, path, result)
}
