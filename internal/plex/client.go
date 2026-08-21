// Package plex adapts the shared github.com/cplieger/plexapi client for the
// exporter. The transport — header-borne token, refuse-all redirects,
// same-origin path guard, CA pinning, transparent retry with Retry-After
// honoring, bounded reads — is the library's; this package owns the
// exporter's construction shape (env-derived CA path, retry counter metric)
// and re-exports the ErrNotFound sentinel the startup classifier and the
// Plex Pass graceful-degradation path key on.
package plex

import (
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/cplieger/plexapi/v2"
)

// Retry defaults preserved from the pre-library client: total attempts
// including the first, and the base backoff between them. The library adds
// a per-attempt response-header timeout so a stalled attempt fails as a
// retryable error.
const (
	defaultMaxAttempts    = 3
	defaultRetryBaseDelay = 100 * time.Millisecond
	// defaultRequestTimeout bounds a request when the caller's context has
	// no deadline (the library never undercuts a caller deadline). Mirrors
	// the old 30s total-retry ceiling.
	defaultRequestTimeout = 30 * time.Second
)

// ErrNotFound is the library's 404 sentinel, re-exported so call sites and
// the Plex Pass graceful-degradation path keep reading plex.ErrNotFound.
// Status-code classification needs no re-export: callers match the
// library's plexapi.StatusError / plexapi.IsConfigError directly.
var ErrNotFound = plexapi.ErrNotFound

// Client is the exporter's Plex client: the library client plus the
// cumulative retry counter surfaced as plex_http_retries_total.
type Client struct {
	*plexapi.Client
	retries *atomic.Int64
}

// NewClient parses serverURL and returns a Client. When caCertPath is
// non-empty, the PEM file at that path is pinned as the sole TLS trust
// anchor (verification stays ON) — the recommended setup for a self-signed
// Plex. An empty caCertPath uses the OS trust store (right for
// *.plex.direct and plain http URLs).
func NewClient(serverURL, token, caCertPath string) (*Client, error) {
	retries := new(atomic.Int64)
	opts := []plexapi.Option{
		plexapi.WithMaxAttempts(defaultMaxAttempts),
		plexapi.WithBaseDelay(defaultRetryBaseDelay),
		plexapi.WithTimeout(defaultRequestTimeout),
		plexapi.WithOnRetry(func(int, *http.Request, *http.Response, error) {
			retries.Add(1)
		}),
	}
	if caCertPath != "" {
		pemBytes, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading PLEX_CA_CERT_PATH=%q: %w", caCertPath, err)
		}
		opts = append(opts, plexapi.WithCACertPEM(pemBytes))
	}
	api, err := plexapi.New(serverURL, token, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: api, retries: retries}, nil
}

// NewClientFromHTTP builds a Client over a caller-supplied *http.Client —
// the test-fixture path (httptest servers). No retry transport or counter
// is installed; Retries() reports 0.
func NewClientFromHTTP(serverURL, token string, hc *http.Client) (*Client, error) {
	api, err := plexapi.New(serverURL, token, plexapi.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Client{Client: api, retries: new(atomic.Int64)}, nil
}

// Retries returns the cumulative number of HTTP retry attempts across all
// requests on this client (the plex_http_retries_total metric).
func (c *Client) Retries() int64 {
	if c.retries == nil {
		return 0
	}
	return c.retries.Load()
}
