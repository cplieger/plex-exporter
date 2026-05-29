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

	"plex-exporter/internal/plexapi"
)

// MaxResponseBody caps the bytes we read from a Plex HTTP response to
// prevent OOM on unexpected payloads. 10 MiB covers the largest realistic
// /status/sessions and /library responses with headroom.
const MaxResponseBody = 10 << 20 // 10 MB

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

// RetryConfig holds retry parameters for GetWithRetry. Zero values mean
// "use package defaults" (100ms base delay).
type RetryConfig struct {
	BaseDelay  time.Duration
	MaxRetries int
}

// Client is the Plex HTTP client. Fields are exported so the internal
// composition root (package main, package server, package wsclient) can
// construct test fixtures and read configuration without accessor noise;
// the whole internal/* tree is a single trust boundary.
type Client struct {
	HTTPClient *http.Client
	BaseURL    *url.URL
	Token      string
	Retry      RetryConfig
}

// NewClient parses serverURL and returns a Client configured with a safe
// default HTTP transport. When caCertPath is non-empty, the PEM file at
// that path is loaded into the TLS RootCAs pool — TLS verification stays
// ENABLED, pinned to that CA. This is the recommended setup for users
// running Plex with a self-signed certificate.
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
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		// Plex's API does not issue redirects; refuse to follow any. Go's
		// default CheckRedirect forwards custom headers (including
		// X-Plex-Token) on cross-origin redirects, which would leak the
		// token to an attacker-controlled host if Plex ever served a 302.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if caCertPath != "" {
		pemBytes, readErr := os.ReadFile(caCertPath)
		if readErr != nil {
			return nil, fmt.Errorf("reading PLEX_CA_CERT_PATH=%q: %w", caCertPath, readErr)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("PLEX_CA_CERT_PATH=%q: no PEM-encoded certificates found", caCertPath)
		}
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		}
	}
	return &Client{BaseURL: parsed, Token: token, HTTPClient: httpClient}, nil
}

// Get fetches path and unmarshals the JSON body into result. Returns
// ErrNotFound for 404, *HTTPStatusError for other non-2xx.
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

// GetWithRetry calls Get with exponential backoff (retryBaseDelay * 2^attempt).
// 4xx client errors (except 429) are not retried. 404 is returned as
// ErrNotFound on the first attempt. Context cancellation during backoff
// returns ctx.Err(); cancellation after at least one failed attempt returns
// the last error so callers see the underlying cause.
func (c *Client) GetWithRetry(ctx context.Context, path string, result any, maxRetries int) error {
	var lastErr error
	for attempt := range maxRetries {
		if ctx.Err() != nil {
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		}
		lastErr = c.Get(ctx, path, result)
		if lastErr == nil {
			return nil
		}
		// Do not retry 4xx client errors (400/401/403 etc.): the request
		// will not succeed with the same token or path. 404 is already
		// mapped to ErrNotFound above. 429 is treated as retryable.
		if se, ok := errors.AsType[*HTTPStatusError](lastErr); ok && se.Code >= 400 && se.Code < 500 && se.Code != http.StatusTooManyRequests {
			return lastErr
		}
		if attempt < maxRetries-1 {
			delay := time.NewTimer(c.retryBaseDelay() * time.Duration(1<<attempt))
			select {
			case <-delay.C:
			case <-ctx.Done():
				delay.Stop()
				return ctx.Err()
			}
		}
	}
	return lastErr
}

func (c *Client) retryBaseDelay() time.Duration {
	if c.Retry.BaseDelay != 0 {
		return c.Retry.BaseDelay
	}
	return 100 * time.Millisecond
}
