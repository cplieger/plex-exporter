package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/sessions"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	movies = library.Library{ID: "1", Name: "Movies", Type: library.TypeMovie}
	shows  = library.Library{ID: "2", Name: "TV", Type: library.TypeShow}
)

func libMatch(id string, extra ...string) map[string]string {
	m := map[string]string{"library_id": id}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

// manyMovies returns n movie rows keyed from base, so a library spans
// several walk pages.
func manyMovies(base, n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = movieRow(base+i, 1000+i)
	}
	return rows
}

func neverBytes(t *testing.T, srv *Server, lib string) (float64, bool) {
	t.Helper()
	return valueOf(t, srv, metrics.DescWatchAgeBytes, libMatch(lib, "last_watched", "never"))
}

func TestWalkPass_publishes_largest_items_with_last_played(t *testing.T) {
	fp := newFakePlex()
	played := time.Now().Add(-time.Hour).Unix()
	fp.libs["1"] = []string{movieRow(10, 500), movieRow(11, 900)}
	fp.history = []historyRow{{RatingKey: "10", ViewedAt: played}}
	srv := newWalkServer(t, fp, movies)
	bootstrap(t, srv, history.StatusComplete)
	srv.walkPass(t.Context(), false)

	if v, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("1", "rating_key", "11", "title", "Movie 11", "year", "2001")); !ok || v != 900 {
		t.Errorf("top item 11 bytes = %v (present %v), want 900", v, ok)
	}
	if v, _ := valueOf(t, srv, metrics.DescTopItemLastPlayed, libMatch("1", "rating_key", "10")); v != float64(played) {
		t.Errorf("top item 10 last played = %v, want %d", v, played)
	}
	if v, ok := valueOf(t, srv, metrics.DescTopItemLastPlayed, libMatch("1", "rating_key", "11")); !ok || v != 0 {
		t.Errorf("top item 11 last played = %v (present %v), want 0 for no recorded play", v, ok)
	}
	if v, _ := valueOf(t, srv, metrics.DescWatchAgeComplete, nil); v != 1 {
		t.Errorf("watch_age_complete = %v, want 1", v)
	}
	if v, _ := valueOf(t, srv, metrics.DescWalkComplete, nil); v != 1 {
		t.Errorf("walk_complete = %v, want 1", v)
	}
}

// A failed library keeps its previous figures and its watched keys, and an
// item added and played while a pass runs keeps its play.
func TestWalkPass_failed_library_keeps_its_watched_set(t *testing.T) {
	fp := newFakePlex()
	now := time.Now()
	fp.libs["1"] = manyMovies(100, walkPageSize+5)
	fp.libs["2"] = []string{movieRow(900, 10)}
	// Item 602 sits on the second page, the one that fails below.
	fp.history = []historyRow{{RatingKey: "602", ViewedAt: now.Add(-48 * time.Hour).Unix()}, {RatingKey: "900", ViewedAt: now.Add(-time.Hour).Unix()}}
	other := library.Library{ID: "2", Name: "Other", Type: library.TypeMovie}
	srv := newWalkServer(t, fp, movies, other)
	bootstrap(t, srv, history.StatusComplete)
	srv.walkPass(t.Context(), false)
	before, _ := neverBytes(t, srv, "1")

	fp.do(func(f *fakePlex) { f.failLib["1"] = true })
	srv.walkPass(t.Context(), false)
	if got, _ := neverBytes(t, srv, "1"); got != before {
		t.Errorf("never bytes after a failed walk = %v, want the previous %v", got, before)
	}
	if srv.History.Visit(602, 0) == 0 {
		t.Error("Visit(602, 0) = 0 after a pass where its library failed, want its play kept")
	}
	if v, _ := valueOf(t, srv, metrics.DescWalkComplete, nil); v != 0 {
		t.Errorf("walk_complete = %v after a failed library, want 0", v)
	}

	// A new item lands in library 1, already read this pass, and is played
	// while library 2 is being read.
	fp.do(func(f *fakePlex) {
		f.failLib["1"] = false
		f.onPage = func(lib string, _ int) {
			if lib == "2" {
				f.do(func(f *fakePlex) {
					f.onPage = nil
					f.libs["1"] = append(f.libs["1"], movieRow(901, 10))
					f.history = append(f.history, historyRow{RatingKey: "901", ViewedAt: time.Now().Unix() + 5})
				})
				if err := srv.History.Incremental(t.Context()); err != nil {
					t.Errorf("Incremental() error = %v", err)
				}
			}
		}
	})
	srv.walkPass(t.Context(), false)
	if got, _ := neverBytes(t, srv, "1"); got != before {
		t.Errorf("never bytes after the recovered walk = %v, want %v", got, before)
	}
	if srv.History.Visit(901, 0) == 0 {
		t.Error("Visit(901, 0) = 0, want the play made during the pass kept")
	}
}

// Watch figures publish only from a walk stamped with the history
// generation that is current now.
func TestWalkPass_watch_figures_wait_for_a_walk_at_the_current_generation(t *testing.T) {
	setup := func(t *testing.T) (*fakePlex, *Server) {
		fp := newFakePlex()
		fp.libs["1"] = []string{movieRow(10, 500)}
		fp.history = []historyRow{{RatingKey: "10", ViewedAt: time.Now().Add(-time.Hour).Unix()}}
		return fp, newWalkServer(t, fp, movies)
	}
	absent := func(t *testing.T, srv *Server, when string) {
		t.Helper()
		if v, ok := neverBytes(t, srv, "1"); ok {
			t.Errorf("%s: watch-age series present (%v), want absent", when, v)
		}
		if _, ok := valueOf(t, srv, metrics.DescTopItemLastPlayed, libMatch("1")); ok {
			t.Errorf("%s: last-played series present, want absent", when)
		}
		if v, _ := valueOf(t, srv, metrics.DescWatchAgeComplete, nil); v != 0 {
			t.Errorf("%s: watch_age_complete = %v, want 0", when, v)
		}
	}

	t.Run("walked_while_history_failed", func(t *testing.T) {
		fp, srv := setup(t)
		bootstrap(t, srv, history.StatusComplete)
		srv.walkPass(t.Context(), false)
		fp.do(func(f *fakePlex) { f.historyFail = true })
		bootstrap(t, srv, history.StatusFailed)
		srv.walkPass(t.Context(), false)
		fp.do(func(f *fakePlex) { f.historyFail = false })
		bootstrap(t, srv, history.StatusComplete)
		absent(t, srv, "after recovery, before the re-walk")
		srv.walkPass(t.Context(), true)
		if _, ok := neverBytes(t, srv, "1"); !ok {
			t.Error("after the re-walk: watch-age series absent, want present")
		}
	})

	t.Run("old_nonzero_stamp", func(t *testing.T) {
		fp, srv := setup(t)
		bootstrap(t, srv, history.StatusComplete)
		srv.walkPass(t.Context(), false)
		if srv.stampOf("1") == 0 {
			t.Fatal("Setup: the first walk was not stamped")
		}
		fp.do(func(f *fakePlex) { f.historyFail = true })
		bootstrap(t, srv, history.StatusFailed)
		fp.do(func(f *fakePlex) { f.historyFail = false })
		bootstrap(t, srv, history.StatusComplete)
		absent(t, srv, "stamped by an older complete generation")
	})

	t.Run("bootstrap_during_the_walk", func(t *testing.T) {
		fp, srv := setup(t)
		fp.libs["1"] = manyMovies(10, walkPageSize+1)
		bootstrap(t, srv, history.StatusComplete)
		fp.do(func(f *fakePlex) {
			f.onPage = func(_ string, start int) {
				if start > 0 {
					if st := srv.History.Bootstrap(t.Context()); st != history.StatusComplete {
						t.Errorf("Bootstrap() during the walk = %q, want complete", st)
					}
				}
			}
		})
		srv.walkPass(t.Context(), false)
		if got := srv.stampOf("1"); got != 0 {
			t.Errorf("stamp = %d for a walk spanning a rebuild, want 0", got)
		}
		absent(t, srv, "walk spanning a rebuild")
	})
}

// With no library to read, unmatched history rows alone decide that the
// watch figures are incomplete.
func TestWalkPass_unmatched_history_rows_without_libraries_read_incomplete(t *testing.T) {
	cases := []struct {
		name    string
		history []historyRow
		want    float64
	}{
		{name: "all_rows_matched", history: []historyRow{{RatingKey: "10", ViewedAt: 100}}, want: 1},
		{name: "one_row_unmatched", history: []historyRow{{RatingKey: "10", ViewedAt: 100}, {RatingKey: "x5", ViewedAt: 5}}, want: 0},
		{name: "one_deleted_item_row", history: []historyRow{{RatingKey: "10", ViewedAt: 100}, {ViewedAt: 5}}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakePlex()
			fp.history = tc.history
			srv := newWalkServer(t, fp)
			bootstrap(t, srv, history.StatusComplete)
			srv.walkPass(t.Context(), false)
			if v, _ := valueOf(t, srv, metrics.DescWatchAgeComplete, nil); v != tc.want {
				t.Errorf("watch_age_complete with no libraries = %v, want %v", v, tc.want)
			}
		})
	}
}

func TestWalkPass_unmatched_history_rows_hide_watch_completeness(t *testing.T) {
	fp := newFakePlex()
	fp.libs["1"] = []string{movieRow(10, 500)}
	fp.history = []historyRow{{RatingKey: "10", ViewedAt: time.Now().Unix()}, {RatingKey: "-1", ViewedAt: 5}, {RatingKey: "11"}}
	srv := newWalkServer(t, fp, movies)
	bootstrap(t, srv, history.StatusComplete)
	srv.walkPass(t.Context(), false)
	if v, _ := valueOf(t, srv, metrics.DescHistoryUnmatched, nil); v != 2 {
		t.Errorf("history_unmatched_rows = %v, want 2", v)
	}
	if v, _ := valueOf(t, srv, metrics.DescWatchAgeComplete, nil); v != 0 {
		t.Errorf("watch_age_complete = %v with unmatched rows, want 0", v)
	}
	// An unmatched row may be a play of any item, so no item may read as
	// never played and no watch-age figure may publish.
	if got := series(t, srv, metrics.DescTopItemLastPlayed); len(got) != 0 {
		t.Errorf("top-item last-played series with unmatched rows = %v, want none", got)
	}
	for _, d := range []*prometheus.Desc{metrics.DescWatchAgeBytes, metrics.DescWatchAgeItems} {
		if got := series(t, srv, d); len(got) != 0 {
			t.Errorf("%s with unmatched rows = %v, want none", d, got)
		}
	}
	if _, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("1", "rating_key", "10")); !ok {
		t.Error("top item 10 bytes absent, want the size published without its last play")
	}
}

// A fixture with the live history shape: plays of deleted items carry no
// ratingKey and some rows share one viewedAt second. Deleted-item rows are
// counted apart and never hide the watch figures.
func TestWalkPass_deleted_item_history_rows_keep_watch_figures(t *testing.T) {
	fp := newFakePlex()
	fp.libs["1"] = []string{movieRow(10, 500), movieRow(11, 900)}
	fp.history = loadHistory(t, "history-live-shape.json")
	srv := newWalkServer(t, fp, movies)
	bootstrap(t, srv, history.StatusComplete)
	srv.walkPass(t.Context(), false)
	if v, _ := valueOf(t, srv, metrics.DescHistoryUnmatched, nil); v != 0 {
		t.Errorf("history_unmatched_rows = %v, want 0: a row without an item key is a deleted item", v)
	}
	if v, _ := valueOf(t, srv, metrics.DescHistoryDeleted, nil); v != 4 {
		t.Errorf("history_deleted_item_rows = %v, want 4", v)
	}
	if v, _ := valueOf(t, srv, metrics.DescWatchAgeComplete, nil); v != 1 {
		t.Errorf("watch_age_complete = %v with only deleted-item rows unusable, want 1", v)
	}
	for _, key := range []string{"10", "11"} {
		if v, ok := valueOf(t, srv, metrics.DescTopItemLastPlayed, libMatch("1", "rating_key", key)); v != 1700000200 {
			t.Errorf("top item %s last played = %v (present %v), want 1700000200 from the shared second", key, v, ok)
		}
	}
	if got := series(t, srv, metrics.DescWatchAgeItems); len(got) == 0 {
		t.Error("watch-age item series absent, want published")
	}
}

// A library whose byte total does not fit int64 fails its walk and keeps
// the figures of its last good walk.
func TestWalkPass_byte_total_overflow_keeps_the_last_figures(t *testing.T) {
	fp := newFakePlex()
	fp.libs["1"] = []string{movieRow(10, 500)}
	srv := newWalkServer(t, fp, movies)
	srv.walkPass(t.Context(), false)

	half := uint64(1)<<62 + 1
	fp.do(func(f *fakePlex) {
		f.libs["1"] = []string{
			fmt.Sprintf(`{"ratingKey":"20","title":"A","Media":[{"Part":[{"size":%d}]}]}`, half),
			fmt.Sprintf(`{"ratingKey":"21","title":"B","Media":[{"Part":[{"size":%d}]}]}`, half),
		}
	})
	srv.walkPass(t.Context(), false)
	if n := srv.ErrorCounts["library_walk"]; n != 1 {
		t.Errorf("library_walk errors = %v, want 1", n)
	}
	if v, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("1", "rating_key", "10")); !ok || v != 500 {
		t.Errorf("top item 10 bytes = %v (present %v), want the last good 500", v, ok)
	}
	if _, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("1", "rating_key", "20")); ok {
		t.Error("an item from the overflowing walk is published, want none")
	}
	if v, _ := valueOf(t, srv, metrics.DescWalkComplete, nil); v != 0 {
		t.Errorf("walk_complete = %v after an overflowing walk, want 0", v)
	}
}

func TestWalkPass_unsized_item_is_never_ranked(t *testing.T) {
	fp := newFakePlex()
	fp.libs["1"] = []string{movieRow(10, 500), `{"ratingKey":"11","title":"No size","Media":[{"Part":[{"id":1}]}]}`}
	srv := newWalkServer(t, fp, movies)
	srv.walkPass(t.Context(), false)
	if _, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("1", "rating_key", "11")); ok {
		t.Error("unsized item 11 is in the largest items, want it left out")
	}
	if v, _ := valueOf(t, srv, metrics.DescUnsizedItems, libMatch("1")); v != 1 {
		t.Errorf("unsized_items = %v, want 1", v)
	}
}

func TestWalkPass_recently_added_lists_ten_per_server(t *testing.T) {
	fp := newFakePlex()
	var rows []string
	for i := range 15 {
		rows = append(rows, fmt.Sprintf(`{"ratingKey":"%d","title":"M%d","addedAt":%d,"Media":[{"Part":[{"size":1}]}]}`, i+1, i+1, 1_700_000_000+i))
	}
	fp.libs["1"] = rows
	fp.libs["2"] = []string{`{"ratingKey":"50","grandparentRatingKey":"40","grandparentTitle":"Show","parentIndex":2,"index":3,"addedAt":1800000000,"Media":[{"Part":[{"size":1}]}]}`}
	srv := newWalkServer(t, fp, movies, shows)
	srv.walkPass(t.Context(), false)
	got := series(t, srv, metrics.DescRecentItemAdded)
	if len(got) != 10 {
		t.Fatalf("recently added series = %d, want 10", len(got))
	}
	if v, ok := valueOf(t, srv, metrics.DescRecentItemAdded, libMatch("2", "rating_key", "50", "title", "Show", "episode", "S02E03")); !ok || v != 1_800_000_000 {
		t.Errorf("newest episode = %v (present %v), want 1800000000", v, ok)
	}
	if _, ok := valueOf(t, srv, metrics.DescRecentItemAdded, libMatch("1", "rating_key", "1")); ok {
		t.Error("the oldest movie is listed, want only the 10 newest")
	}
}

// An episode row carries no show year, so a listed show takes its year
// from the show's own metadata, read once and kept across passes.
func TestWalkPass_listed_shows_carry_the_show_year(t *testing.T) {
	fp := newFakePlex()
	fp.libs["2"] = []string{`{"ratingKey":"50","grandparentRatingKey":"40","grandparentTitle":"Show","year":2015,"parentIndex":2,"index":3,"addedAt":1800000000,"Media":[{"Part":[{"size":7}]}]}`}
	fp.showYears["40"] = 2008
	srv := newWalkServer(t, fp, shows)
	srv.walkPass(t.Context(), false)
	if _, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("2", "rating_key", "40", "year", "2008")); !ok {
		t.Errorf("largest show 40 series = %v, want year 2008", series(t, srv, metrics.DescTopItemBytes))
	}
	if _, ok := valueOf(t, srv, metrics.DescRecentItemAdded, libMatch("2", "rating_key", "50", "year", "2008")); !ok {
		t.Errorf("recently added show series = %v, want year 2008", series(t, srv, metrics.DescRecentItemAdded))
	}
	fp.do(func(f *fakePlex) { delete(f.showYears, "40"); f.metaReads = 0 })
	srv.walkPass(t.Context(), false)
	if _, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("2", "rating_key", "40", "year", "2008")); !ok {
		t.Errorf("after a second pass largest show 40 series = %v, want year 2008 kept", series(t, srv, metrics.DescTopItemBytes))
	}
	fp.do(func(f *fakePlex) {
		if f.metaReads != 0 {
			t.Errorf("second pass read show metadata %d times, want 0 for a show whose year is known", f.metaReads)
		}
	})
}

// A show whose metadata cannot be read is listed without a year, and the
// failed read is counted.
func TestWalkPass_failed_show_year_read_is_counted(t *testing.T) {
	fp := newFakePlex()
	fp.libs["2"] = []string{`{"ratingKey":"50","grandparentRatingKey":"40","grandparentTitle":"Show","parentIndex":1,"index":1,"addedAt":1800000000,"Media":[{"Part":[{"size":7}]}]}`}
	srv := newWalkServer(t, fp, shows)
	srv.walkPass(t.Context(), false)
	if n := srv.ErrorCounts["metadata_fetch"]; n != 1 {
		t.Errorf("metadata_fetch errors = %v, want 1 for the one unreadable show", n)
	}
	if _, ok := valueOf(t, srv, metrics.DescTopItemBytes, libMatch("2", "rating_key", "40", "year", "")); !ok {
		t.Errorf("largest show 40 series = %v, want an empty year", series(t, srv, metrics.DescTopItemBytes))
	}
}

func TestWalkPass_title_label_is_bounded(t *testing.T) {
	fp := newFakePlex()
	title, err := json.Marshal(strings.Repeat("é", 5000) + "\x1b[31m")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	fp.libs["1"] = []string{fmt.Sprintf(`{"ratingKey":"1","title":%s,"Media":[{"Part":[{"size":9}]}]}`, title)}
	srv := newWalkServer(t, fp, movies)
	srv.walkPass(t.Context(), false)
	got := series(t, srv, metrics.DescTopItemBytes)
	if len(got) != 1 {
		t.Fatalf("top item series = %d, want 1", len(got))
	}
	if l := got[0].labels["title"]; len(l) > maxLabelLen || strings.ContainsRune(l, 0x1b) {
		t.Errorf("title label is %d bytes (escape kept %v), want at most %d and no control runes", len(l), strings.ContainsRune(l, 0x1b), maxLabelLen)
	}
}

func TestWalkPass_skips_libraries_past_the_limit(t *testing.T) {
	fp := newFakePlex()
	var libs []library.Library
	for i := range maxWalkedLibraries + 2 {
		id := fmt.Sprint(i + 1)
		libs = append(libs, library.Library{ID: id, Type: library.TypeMovie})
		fp.libs[id] = []string{movieRow(1000+i, 1)}
	}
	srv := newWalkServer(t, fp, libs...)
	srv.walkPass(t.Context(), false)
	if v, _ := valueOf(t, srv, metrics.DescWalkSkipped, nil); v != 2 {
		t.Errorf("walk_skipped_libraries = %v, want 2", v)
	}
	if n := len(series(t, srv, metrics.DescWalkable)); n != maxWalkedLibraries {
		t.Errorf("walkable series = %d, want %d", n, maxWalkedLibraries)
	}
	if v, _ := valueOf(t, srv, metrics.DescWalkComplete, nil); v != 0 {
		t.Errorf("walk_complete = %v with skipped libraries, want 0", v)
	}
}

// A pedantic registry rejects any collected metric whose descriptor
// Describe did not send, so every family must be in metrics.AllDescs.
func TestCollect_every_emitted_family_is_described(t *testing.T) {
	fp := newFakePlex()
	fp.sections = twoSections
	fp.activities = `{"MediaContainer":{"Activity":[{"uuid":"a","type":"library.update.section","progress":40,"Context":{"librarySectionID":"1"}}]}}`
	fp.updater = `{"MediaContainer":{"checkedAt":1700000000,"status":0,"Release":[{"version":"2","state":"available"}]}}`
	fp.libs["1"] = []string{movieRow(10, 500)}
	fp.history = []historyRow{{RatingKey: "10", ViewedAt: time.Now().Unix()}}
	srv := newWalkServer(t, fp, library.Library{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 1, ItemsKnown: true})
	srv.Version, srv.ResourcesRead, srv.BandwidthRead = "1", true, true
	srv.refreshBackground(t.Context())
	bootstrap(t, srv, history.StatusComplete)
	srv.walkPass(t.Context(), false)
	meta := testMeta(t, `{"sessionKey":"s1","Session":{"bandwidth":900},"Media":[{"bitrate":800}]}`)
	srv.Sessions.Update("s1", sessions.StatePlaying, &meta, nil)
	srv.Sessions.UpdateLibraryLabels("s1", func(ss *sessions.Session) {
		ss.VideoTranscoding, ss.VideoDecode, ss.VideoEncode = true, "hardware", "hardware"
	})

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(srv); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != len(metrics.AllDescs) {
		t.Errorf("gathered %d families, want every one of the %d in AllDescs", len(families), len(metrics.AllDescs))
	}
}

// An item added before the read offset shifts the next page, so the last
// row of the first page is served again; the gather must still succeed.
func TestWalkPass_row_repeated_across_pages_is_emitted_once(t *testing.T) {
	fp := newFakePlex()
	fp.libs["1"] = manyMovies(100, walkPageSize+5)
	fp.onPage = func(lib string, start int) {
		if start == 0 {
			fp.do(func(f *fakePlex) { f.libs[lib] = append([]string{movieRow(99, 1)}, f.libs[lib]...) })
		}
	}
	srv := newWalkServer(t, fp, movies)
	srv.walkPass(t.Context(), false)

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(srv); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Errorf("Gather() error = %v, want none", err)
	}
	// The 505 movies are sized 1000 to 1504 bytes.
	if v, _ := valueOf(t, srv, metrics.DescResolutionBytes, libMatch("1", "video_resolution", "1080")); v != 632260 {
		t.Errorf("1080 bytes = %v, want 632260 with each item once", v)
	}
	if n := len(series(t, srv, metrics.DescTopItemBytes)); n != 10 {
		t.Errorf("top item series = %d, want 10", n)
	}
}

func TestRunHistoryLoop_without_history_returns(t *testing.T) {
	srv := &Server{}
	srv.RunHistoryLoop(t.Context())
}
