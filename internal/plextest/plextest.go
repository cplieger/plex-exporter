// Package plextest provides test helpers for building a plex.Client wired
// to an httptest.Server, kept out of package plex so the production binary
// never links testing/httptest.
package plextest

import (
	"net/http/httptest"
	"testing"

	"github.com/cplieger/plex-exporter/v2/internal/plex"
)

// TestToken is the fixed credential used by NewTestClientFromServer and
// hand-rolled fixtures. The leading "$" mimics an unexpanded env-var
// placeholder the repo secret-scan regex excludes.
const TestToken = "$fixture-test-token"

// NewTestClientFromServer constructs a *plex.Client wired to ts, using the
// server's own HTTP client for proper TLS/transport handling.
func NewTestClientFromServer(t testing.TB, ts *httptest.Server) *plex.Client {
	t.Helper()
	c, err := plex.NewClientFromHTTP(ts.URL, TestToken, ts.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c
}
