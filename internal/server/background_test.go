package server

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plextest"
	"github.com/cplieger/plexapi/v2"
)

const twoSections = `{"MediaContainer":{"Directory":[
	{"key":"1","title":"Movies","type":"movie","scannedAt":1700000000},
	{"key":"2","title":"TV","type":"show"}]}}`

func TestRefreshSections_scan_time_absent_leaves_no_series(t *testing.T) {
	fp := newFakePlex()
	fp.sections = twoSections
	srv := newWalkServer(t, fp, movies, shows)
	srv.refreshSections(t.Context())
	got := series(t, srv, metrics.DescLastScan)
	if len(got) != 1 || got[0].labels["library_id"] != "1" || got[0].value != 1700000000 {
		t.Errorf("last_scan series = %+v, want only library 1 at 1700000000", got)
	}
}

// Two activity types in one library and one server-wide task give three
// series; a library is counted once however many tasks it runs.
func TestRefreshActivities_maps_types_and_libraries(t *testing.T) {
	fp := newFakePlex()
	fp.sections = twoSections
	fp.activities = `{"MediaContainer":{"Activity":[
		{"uuid":"a","type":"library.update.section","progress":40,"Context":{"librarySectionID":"1"}},
		{"uuid":"b","type":"media.generate.credits","progress":-1,"Context":{"librarySectionID":1}},
		{"uuid":"c","type":"butler.optimize","progress":10},
		{"uuid":"d","type":"library.update.section","progress":20,"Context":{"librarySectionID":"1"}},
		{"uuid":"e","type":"media.generate.bif","Context":{"librarySectionID":"2"}},
		{"uuid":"f","type":"library.update.section","progress":7000,"Context":{"librarySectionID":"2"}}]}}`
	srv := newWalkServer(t, fp, movies, shows)
	srv.refreshSections(t.Context())
	srv.refreshActivities(t.Context())

	want := map[string]float64{"scan/1": 0.4, "analysis/1": -1, "maintenance/": 0.1}
	got := map[string]float64{}
	for _, s := range series(t, srv, metrics.DescActivityProgress) {
		got[s.labels["activity_type"]+"/"+s.labels["library_id"]] = s.value
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("activity series = %v, want %v (absent progress and 7000%% dropped)", got, want)
	}
}

func TestRefreshActivities_failed_reads_stop_publishing_the_last_answer(t *testing.T) {
	fp := newFakePlex()
	fp.sections = twoSections
	fp.activities = `{"MediaContainer":{"Activity":[
		{"uuid":"a","type":"library.update.section","progress":40,"Context":{"librarySectionID":"1"}}]}}`
	srv := newWalkServer(t, fp, movies, shows)
	srv.refreshSections(t.Context())
	srv.refreshActivities(t.Context())
	if got := series(t, srv, metrics.DescActivityProgress); len(got) != 1 {
		t.Fatalf("Setup: activity series after a good read = %d, want 1", len(got))
	}

	fp.do(func(f *fakePlex) { f.activities = "" })
	srv.refreshActivities(t.Context())
	if got := series(t, srv, metrics.DescActivityProgress); len(got) != 1 {
		t.Errorf("activity series after one failed read = %d, want 1: one miss is tolerated", len(got))
	}

	srv.mu.Lock()
	srv.background.activitiesAt = time.Now().Add(-activitiesStaleAfter - time.Second)
	srv.mu.Unlock()
	srv.refreshActivities(t.Context())
	if got := series(t, srv, metrics.DescActivityProgress); len(got) != 0 {
		t.Errorf("activity series once the last good read is %v old = %v, want none", activitiesStaleAfter, got)
	}
	if n := srv.ErrorCounts["activities_fetch"]; n != 2 {
		t.Errorf("activities_fetch errors = %v, want 2", n)
	}
}

func TestRefreshActivities_hostile_answer_stays_bounded(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var acts []string
	for i := range 1000 {
		acts = append(acts, fmt.Sprintf(`{"uuid":"%d","type":%q,"title":%q,"progress":%d,"Context":{"librarySectionID":"%d"}}`,
			i, strings.Repeat("x", r.IntN(40)), strings.Repeat("t", 10_000), r.IntN(100), r.IntN(1_000_000)))
	}
	fp := newFakePlex()
	fp.sections = twoSections
	fp.activities = `{"MediaContainer":{"Activity":[` + strings.Join(acts, ",") + `]}}`
	srv := newWalkServer(t, fp, movies, shows)
	srv.refreshSections(t.Context())
	srv.refreshActivities(t.Context())
	got := series(t, srv, metrics.DescActivityProgress)
	if limit := 6 * (2 + 1); len(got) > limit {
		t.Errorf("activity series = %d, want at most %d", len(got), limit)
	}
	for _, s := range got {
		for k, v := range s.labels {
			if strings.Contains(v, "tttt") {
				t.Errorf("label %s carries an activity title", k)
			}
		}
	}
}

// The background reads never fail a refresh: a 404 and a hang on
// /activities leave the refresh, and so plex_http_reachable, healthy.
func TestRefresh_survives_a_failing_or_hanging_activities_read(t *testing.T) {
	saved := backgroundDeadline
	backgroundDeadline = 200 * time.Millisecond
	t.Cleanup(func() { backgroundDeadline = saved })
	for _, hang := range []bool{false, true} {
		t.Run(fmt.Sprintf("hang=%v", hang), func(t *testing.T) {
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/media/providers":
					fmt.Fprint(w, `{"MediaContainer":{"machineIdentifier":"m","friendlyName":"f","version":"1"}}`)
				case "/":
					fmt.Fprint(w, `{"MediaContainer":{"platform":"Linux"}}`)
				case "/activities":
					if hang {
						<-release
					}
					w.WriteHeader(http.StatusNotFound)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer ts.Close()
			defer close(release)
			srv := New(plextest.NewTestClientFromServer(t, ts))
			srv.Pace = nil
			if err := srv.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh() error = %v, want nil", err)
			}
			srv.mu.Lock()
			n := srv.ErrorCounts["activities_fetch"]
			srv.mu.Unlock()
			if n != 1 {
				t.Errorf("activities_fetch errors = %v, want 1", n)
			}
		})
	}
}

func TestDecideUpdate(t *testing.T) {
	const running = "1.40.2.8395-c67dce28e"
	rel := func(version, state string) plexapi.UpdateRelease {
		return plexapi.UpdateRelease{Version: version, State: state}
	}
	checked := new(plexapi.FlexInt64(1_700_000_000))
	tests := []struct {
		name      string
		st        plexapi.UpdateStatus
		running   string
		known     bool
		available float64
		state     string
	}{
		{"no release", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0)}, running, true, 0, "none"},
		{"available", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel("1.41", "available")}}, running, true, 1, "available"},
		{"notify", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel("1.41", "notify")}}, running, true, 1, "notify"},
		{"skipped is still behind", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel("1.41", "skipped")}}, running, true, 1, "skipped"},
		{"downloaded", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel("1.41", "downloaded")}}, running, true, 1, "downloaded"},
		{"done", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel("1.41", "done")}}, running, true, 0, "done"},
		{"running version listed", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel(running, "notify")}}, running, true, 0, "notify"},
		{"two releases, higher wins", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel(running, "notify"), rel("1.42", "available")}}, running, true, 1, "available"},
		{"unknown state", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0), Releases: []plexapi.UpdateRelease{rel("1.41", "weird")}}, running, true, 0, "other"},
		{"updater error", plexapi.UpdateStatus{CheckedAt: checked, Status: new(3)}, running, false, 0, ""},
		{"never checked", plexapi.UpdateStatus{CheckedAt: new(plexapi.FlexInt64(0)), Status: new(0)}, running, false, 0, ""},
		{"running version unknown", plexapi.UpdateStatus{CheckedAt: checked, Status: new(0)}, "", false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(strings.ReplaceAll(tt.name, " ", "_"), func(t *testing.T) {
			got := decideUpdate(&tt.st, tt.running)
			if got.known != tt.known || got.available != tt.available || got.state != tt.state {
				t.Errorf("decideUpdate(%s) = %+v, want known=%v available=%v state=%q", tt.name, got, tt.known, tt.available, tt.state)
			}
		})
	}
}

func TestRefreshUpdater_not_found_counts_no_error(t *testing.T) {
	srv := newWalkServer(t, newFakePlex(), movies)
	srv.refreshUpdater(t.Context())
	srv.mu.Lock()
	n := srv.ErrorCounts["updater_fetch"]
	srv.mu.Unlock()
	if n != 0 || len(series(t, srv, metrics.DescUpdateAvailable)) != 0 {
		t.Errorf("updater 404: errors=%v series=%d, want 0 and 0", n, len(series(t, srv, metrics.DescUpdateAvailable)))
	}
}

func TestRefreshUpdater_publishes_the_decision(t *testing.T) {
	fp := newFakePlex()
	fp.updater = `{"MediaContainer":{"checkedAt":1700000000,"status":0,"Release":[{"version":"1.42","state":"available","downloadURL":"https://example.invalid/?X-Plex-Token=secret"}]}}`
	srv := newWalkServer(t, fp, movies)
	srv.Version = "1.40"
	srv.refreshUpdater(t.Context())
	if v, ok := valueOf(t, srv, metrics.DescUpdateAvailable, map[string]string{"state": "available"}); !ok || v != 1 {
		t.Errorf("update_available = %v (present %v), want 1", v, ok)
	}
	if v, _ := valueOf(t, srv, metrics.DescUpdateChecked, nil); v != 1700000000 {
		t.Errorf("update_checked = %v, want 1700000000", v)
	}
	body, _ := json.Marshal(series(t, srv, metrics.DescUpdateAvailable))
	if strings.Contains(string(body), "secret") {
		t.Error("an update series carries the download URL token")
	}
}

// The body a live server sends when no update is available: no Release
// element, plus fields the decoder does not model. The tile reads 0, Up to
// date.
func TestRefreshUpdater_no_update_available_reads_up_to_date(t *testing.T) {
	fp := newFakePlex()
	fp.updater = `{"MediaContainer":{"size":0,"canInstall":false,"checkedAt":1700000000,"status":0}}`
	srv := newWalkServer(t, fp, movies)
	srv.Version = "1.40.2.8395-c67dce28e"
	srv.refreshUpdater(t.Context())
	if got := series(t, srv, metrics.DescUpdateAvailable); len(got) != 1 || got[0].value != 0 || got[0].labels["state"] != "none" {
		t.Errorf("update_available series = %+v, want one series state=none value 0", got)
	}
}

func TestBackgroundReads_wait_for_their_cadence(t *testing.T) {
	fp := newFakePlex()
	fp.sections = twoSections
	srv := newWalkServer(t, fp, movies)
	srv.refreshBackground(t.Context())
	srv.mu.Lock()
	first := srv.background.lastSections
	srv.mu.Unlock()
	srv.refreshBackground(t.Context())
	srv.mu.Lock()
	again := srv.background.lastSections
	srv.mu.Unlock()
	if first.IsZero() || !again.Equal(first) {
		t.Errorf("sections read at %v then %v, want one read inside %v", first, again, sectionsInterval)
	}
}
