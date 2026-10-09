package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plexapi/v2"
)

// testToken is the credential of every test client. The leading "$" mimics
// an unexpanded env-var placeholder the repo secret-scan regex excludes.
const testToken = "$fixture-test-token"

func newTestClient(t testing.TB, ts *httptest.Server) *plex.Client {
	t.Helper()
	return newTestClientAt(t, ts.URL, ts.Client())
}

func newTestClientAt(t testing.TB, serverURL string, hc *http.Client) *plex.Client {
	t.Helper()
	api, err := plexapi.New(serverURL, testToken, plexapi.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	return &plex.Client{Client: api}
}
