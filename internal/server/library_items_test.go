package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/plextest"
)

func TestRefreshLibraryItems_counts_by_type(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections/1/all":
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":150}}`)
		case "/library/sections/2/all":
			// Show library with type=4 (episodes)
			if r.URL.Query().Get("type") == "4" {
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":500}}`)
				return
			}
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":25}}`)
		case "/library/sections/3/all":
			// Artist library: type=10 (tracks) first
			if r.URL.Query().Get("type") == "10" {
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":2000}}`)
				return
			}
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":100}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
		{ID: "2", Name: "TV Shows", Type: library.TypeShow},
		{ID: "3", Name: "Music", Type: library.TypeArtist},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.Libraries[0].ItemsCount != 150 {
		t.Errorf("Movies ItemsCount = %d, want 150", srv.Libraries[0].ItemsCount)
	}
	if srv.Libraries[1].ItemsCount != 500 {
		t.Errorf("TV Shows ItemsCount = %d, want 500 (episodes)", srv.Libraries[1].ItemsCount)
	}
	if srv.Libraries[2].ItemsCount != 2000 {
		t.Errorf("Music ItemsCount = %d, want 2000 (tracks)", srv.Libraries[2].ItemsCount)
	}
}

func TestRefreshLibraryItems_writeback_boundary(t *testing.T) {
	// Refreshed counts are written back to the matching sections, by index and
	// ID, for every library in the list.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/library/sections/1/all"):
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":100}}`)
		case strings.HasPrefix(r.URL.Path, "/library/sections/2/all"):
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":200}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie},
		{ID: "2", Name: "TV", Type: library.TypeShow},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.Libraries[0].ItemsCount != 100 {
		t.Errorf("Movies ItemsCount = %d, want 100", srv.Libraries[0].ItemsCount)
	}
	if srv.Libraries[1].ItemsCount != 200 {
		t.Errorf("TV ItemsCount = %d, want 200", srv.Libraries[1].ItemsCount)
	}
}

func TestRefreshLibraryItems_no_libraries_is_noop(t *testing.T) {
	// With no libraries the worker pool is empty, so refreshLibraryItems makes
	// no HTTP calls and leaves the (empty) library list untouched.
	var hit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = nil

	srv.refreshLibraryItems(context.Background())

	if hit {
		t.Error("refreshLibraryItems made an HTTP call with no libraries")
	}
	if len(srv.Libraries) != 0 {
		t.Errorf("libraries count = %d, want 0", len(srv.Libraries))
	}
}

func TestRefreshLibraryItems_artist_fallback_to_type7(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				// type=10 returns 0 — trigger fallback
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":350}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":50}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Music", Type: library.TypeArtist},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.Libraries[0].ItemsCount != 350 {
		t.Errorf("Music ItemsCount = %d, want 350 (type=7 fallback)", srv.Libraries[0].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_type10_error_falls_back(t *testing.T) {
	// When the type=10 (tracks) query errors, the count falls back to type=7.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				w.WriteHeader(http.StatusInternalServerError)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":777}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":50}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Music", Type: library.TypeArtist},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.Libraries[0].ItemsCount != 777 {
		t.Errorf("Music ItemsCount = %d, want 777 (type=10 error, type=7 fallback)", srv.Libraries[0].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_type10_returns_zero_falls_to_type7(t *testing.T) {
	// When type=10 (tracks) returns count=0 — not an error, just zero — the
	// count is not accepted and falls back to type=7. Zero is the boundary: a
	// positive count is required (count > 0), so 0 falls through.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":500}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":10}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Music", Type: library.TypeArtist},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// type=10 returned 0, so should fall back to type=7 which returns 500
	if srv.Libraries[0].ItemsCount != 500 {
		t.Errorf("Music ItemsCount = %d, want 500 (type=10 returned 0, should use type=7)", srv.Libraries[0].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_type7_returns_zero_falls_to_default(t *testing.T) {
	// When both type=10 and type=7 return 0, should fall through to default path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":99}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Music", Type: library.TypeArtist},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// Both type queries returned 0, should fall through to default path
	if srv.Libraries[0].ItemsCount != 99 {
		t.Errorf("Music ItemsCount = %d, want 99 (both type queries returned 0, default path)", srv.Libraries[0].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_both_fail_uses_default_path(t *testing.T) {
	// When both type=10 and type=7 fail for artist, should fall through to default path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				w.WriteHeader(http.StatusInternalServerError)
			case "7":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Music", Type: library.TypeArtist},
	}

	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.Libraries[0].ItemsCount != 42 {
		t.Errorf("Music ItemsCount = %d, want 42 (both type queries failed, default path)", srv.Libraries[0].ItemsCount)
	}
}

func TestFillItemCount_non_numeric_id_records_error(t *testing.T) {
	var hit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	client := plextest.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	lb := library.Library{ID: "not-numeric", Name: "Bad Section", Type: library.TypeMovie}

	srv.fillItemCount(context.Background(), &lb)

	if hit {
		t.Error("fillItemCount issued an HTTP fetch for a non-numeric section ID; the strconv.Atoi guard must short-circuit first")
	}
	if lb.ItemsCount != 0 {
		t.Errorf("ItemsCount = %d, want 0 (non-numeric ID rejected)", lb.ItemsCount)
	}
	srv.mu.Lock()
	got := srv.ErrorCounts["library_items"]
	srv.mu.Unlock()
	if got != 1 {
		t.Errorf("ErrorCounts[library_items] = %v, want 1", got)
	}
}
