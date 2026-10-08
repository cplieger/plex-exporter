package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/plextest"
	"github.com/prometheus/client_golang/prometheus"
)

// fakePlex serves the paged catalog, watch history and background
// endpoints from in-memory fixtures. Every field is guarded by mu.
type fakePlex struct {
	libs        map[string][]string
	failLib     map[string]bool
	onPage      func(lib string, start int)
	sections    string
	activities  string
	updater     string
	showYears   map[string]int
	history     []historyRow
	historyFail bool
	metaReads   int
	pageCap     int
	mu          sync.Mutex
}

// historyRow is one served history row; a row loaded by loadHistory is
// served as its raw fixture bytes.
type historyRow struct {
	RatingKey string `json:"ratingKey,omitempty"`
	raw       json.RawMessage
	ViewedAt  int64 `json:"viewedAt,omitempty"`
}

func (h historyRow) MarshalJSON() ([]byte, error) {
	if h.raw != nil {
		return h.raw, nil
	}
	type plain historyRow
	return json.Marshal(plain(h))
}

// loadHistory reads a testdata file holding a JSON array of history rows.
func loadHistory(t *testing.T, name string) []historyRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Setup: read %s: %v", name, err)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		t.Fatalf("Setup: decode %s: %v", name, err)
	}
	rows := make([]historyRow, len(raws))
	for i, r := range raws {
		if err := json.Unmarshal(r, &rows[i]); err != nil {
			t.Fatalf("Setup: decode %s row %d: %v", name, i, err)
		}
		rows[i].raw = r
	}
	return rows
}

var historySince = regexp.MustCompile(`viewedAt>=(-?\d+)`)

func newFakePlex() *fakePlex {
	return &fakePlex{libs: map[string][]string{}, failLib: map[string]bool{}, showYears: map[string]int{}}
}

func (f *fakePlex) do(fn func(*fakePlex)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// pageBounds is the window a request asks for, shortened to pageCap rows
// when it is set, as a server that caps its pages answers.
func (f *fakePlex) pageBounds(r *http.Request, n int) (start, end int) {
	q := r.URL.Query()
	start, _ = strconv.Atoi(q.Get("X-Plex-Container-Start"))
	size, _ := strconv.Atoi(q.Get("X-Plex-Container-Size"))
	if f.pageCap > 0 {
		size = min(size, f.pageCap)
	}
	return min(start, n), min(start+size, n)
}

func (f *fakePlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch path := r.URL.Path; {
	case path == "/status/sessions/history/all":
		f.serveHistory(w, r)
	case strings.HasPrefix(path, "/library/sections/") && strings.HasSuffix(path, "/all"):
		lib := strings.TrimSuffix(strings.TrimPrefix(path, "/library/sections/"), "/all")
		f.serveLibrary(w, r, lib)
	case path == "/library/sections" && f.sections != "":
		fmt.Fprint(w, f.sections)
	case path == "/activities" && f.activities != "":
		fmt.Fprint(w, f.activities)
	case path == "/updater/status" && f.updater != "":
		fmt.Fprint(w, f.updater)
	case strings.HasPrefix(path, "/library/metadata/"):
		f.metaReads++
		key := strings.TrimPrefix(path, "/library/metadata/")
		year, ok := f.showYears[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"ratingKey":%q,"type":"show","title":"Show","year":%d}]}}`, key, year)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakePlex) serveLibrary(w http.ResponseWriter, r *http.Request, lib string) {
	rows := f.libs[lib]
	start, end := f.pageBounds(r, len(rows))
	if fn := f.onPage; fn != nil {
		f.mu.Unlock()
		fn(lib, start)
		f.mu.Lock()
	}
	if f.failLib[lib] && start > 0 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, `{"MediaContainer":{"totalSize":%d,"Metadata":[%s]}}`, len(rows), strings.Join(rows[start:end], ","))
}

func (f *fakePlex) serveHistory(w http.ResponseWriter, r *http.Request) {
	if f.historyFail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	since := int64(0)
	if m := historySince.FindStringSubmatch(r.URL.RawQuery); m != nil {
		since, _ = strconv.ParseInt(m[1], 10, 64)
	}
	var sel []historyRow
	for _, h := range f.history {
		if h.ViewedAt >= since || h.ViewedAt == 0 {
			sel = append(sel, h)
		}
	}
	start, end := f.pageBounds(r, len(sel))
	body, _ := json.Marshal(sel[start:end])
	fmt.Fprintf(w, `{"MediaContainer":{"totalSize":%d,"Metadata":%s}}`, len(sel), body)
}

type noPace struct{}

func (noPace) Wait(ctx context.Context) error { return ctx.Err() }

// newWalkServer returns a Server over fp with pacing off and the given
// libraries known.
func newWalkServer(t *testing.T, fp *fakePlex, libs ...library.Library) *Server {
	t.Helper()
	ts := httptest.NewServer(fp)
	t.Cleanup(ts.Close)
	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.Pace = nil
	srv.History = history.New(client, noPace{})
	srv.Name, srv.ID = "srv", "mid"
	srv.Libraries = libs
	return srv
}

// bootstrap runs a full history read and stops the test unless it ends
// with want, so a broken setup is not reported as a later failure.
func bootstrap(t *testing.T, srv *Server, want history.Status) {
	t.Helper()
	if st := srv.History.Bootstrap(t.Context()); st != want {
		t.Fatalf("Setup: Bootstrap() = %q, want %q", st, want)
	}
}

type sample struct {
	labels map[string]string
	value  float64
}

// series collects srv once and returns the samples of d.
func series(t *testing.T, srv *Server, d *prometheus.Desc) []sample {
	t.Helper()
	ch := make(chan prometheus.Metric, 4096)
	srv.Collect(ch)
	close(ch)
	var out []sample
	for _, m := range collectByDesc(ch)[descKey(d)] {
		l, v := metricSnapshot(t, m)
		out = append(out, sample{l, v})
	}
	return out
}

func valueOf(t *testing.T, srv *Server, d *prometheus.Desc, match map[string]string) (float64, bool) {
	t.Helper()
	for _, s := range series(t, srv, d) {
		ok := true
		for k, v := range match {
			ok = ok && s.labels[k] == v
		}
		if ok {
			return s.value, true
		}
	}
	return 0, false
}

func movieRow(key, size int) string {
	return fmt.Sprintf(`{"ratingKey":"%d","title":"Movie %d","year":2001,"addedAt":1700000000,"Media":[{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":%d}]}]}`, key, key, size)
}
