package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/pacer"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/plextest"
)

// shortPage is the row count the fake server answers a WalkPageSize or
// history.PageSize request with, so every page boundary falls between the
// readers' own page multiples. Five such pages hold 1500 rows, more than
// ceil(1500/500)+1 requests: a reader bounded by request count stops short.
const shortPage = 300

// stampedTransport serves fp in process, recording when each request
// starts, so a synctest bubble's fake clock times every request.
type stampedTransport struct {
	fp *fakePlex
	at []time.Time
	mu sync.Mutex
}

func (s *stampedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.at = append(s.at, time.Now())
	s.mu.Unlock()
	rec := httptest.NewRecorder()
	s.fp.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func (s *stampedTransport) requests() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

// pacedServer returns a Server over fp under a real shared pacer, with
// every request stamped by the returned transport.
func pacedServer(t *testing.T, fp *fakePlex) (*Server, *stampedTransport) {
	t.Helper()
	tr := &stampedTransport{fp: fp}
	client, err := plex.NewClientFromHTTP("http://127.0.0.1:32400", plextest.TestToken, &http.Client{Transport: tr})
	if err != nil {
		t.Fatalf("Setup: NewClientFromHTTP: %v", err)
	}
	srv := New(client)
	srv.Pace = pacer.New(pacer.DefaultInterval)
	srv.History = history.New(client, srv.Pace)
	srv.Name, srv.ID = "srv", "mid"
	srv.Libraries = []library.Library{movies}
	return srv, tr
}

// busiestSecond returns the most requests that start inside any one-second
// window.
func busiestSecond(at []time.Time) int {
	most := 0
	for i := range at {
		n := 0
		for _, u := range at {
			if !u.Before(at[i]) && u.Before(at[i].Add(time.Second)) {
				n++
			}
		}
		most = max(most, n)
	}
	return most
}

func TestWalkPass_short_pages_stay_under_two_requests_a_second(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fp := newFakePlex()
		fp.pageCap = shortPage
		fp.libs["1"] = manyMovies(1, 5*shortPage)
		srv, tr := pacedServer(t, fp)
		srv.History = nil
		srv.WalkPass(t.Context(), false)

		// manyMovies sizes rows 1000 to 2499 bytes; their sum is 2624250.
		if v, ok := valueOf(t, srv, metrics.DescResolutionBytes, libMatch("1", "video_resolution", "1080")); !ok || v != 2624250 {
			t.Errorf("1080 bytes after five %d-row pages = %v (present %v), want 2624250", shortPage, v, ok)
		}
		at := tr.requests()
		if len(at) != 5 {
			t.Errorf("catalog page requests = %d, want 5", len(at))
		}
		if n := busiestSecond(at); n > 2 {
			t.Errorf("busiest second of the walk held %d requests (at %v), want at most 2", n, at)
		}
	})
}

func TestBootstrap_short_pages_stay_under_two_requests_a_second(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fp := newFakePlex()
		fp.pageCap = shortPage
		for i := range 5 * shortPage {
			fp.history = append(fp.history, historyRow{RatingKey: "10", ViewedAt: int64(1700000000 + i)})
		}
		srv, tr := pacedServer(t, fp)
		if st := srv.History.Bootstrap(t.Context()); st != history.StatusComplete {
			t.Fatalf("Bootstrap() over five %d-row pages = %q, want complete", shortPage, st)
		}
		if got, want := srv.History.LastPlayed(10), int64(1700000000+5*shortPage-1); got != want {
			t.Errorf("LastPlayed(10) = %d, want %d from the last page", got, want)
		}
		at := tr.requests()
		if len(at) != 5 {
			t.Errorf("history page requests = %d, want 5", len(at))
		}
		if n := busiestSecond(at); n > 2 {
			t.Errorf("busiest second of the history read held %d requests (at %v), want at most 2", n, at)
		}
	})
}
