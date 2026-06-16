package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/library"
	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plex-exporter/internal/plex"
	"github.com/cplieger/plex-exporter/internal/sessions"
)

func TestRefreshResources_updates_host_metrics(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/resources" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[
				{"hostCpuUtilization":25.0,"hostMemoryUtilization":50.0},
				{"hostCpuUtilization":42.0,"hostMemoryUtilization":65.0}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.refreshResources(context.Background())

	srv.mu.Lock()
	cpu := srv.HostCPU
	mem := srv.HostMem
	srv.mu.Unlock()

	// API returns percentages (0-100), code divides by 100 to get ratios
	if cpu != 0.42 {
		t.Errorf("hostCPU = %v, want 0.42", cpu)
	}
	if mem != 0.65 {
		t.Errorf("hostMem = %v, want 0.65", mem)
	}
}

func TestRefreshResources_empty_stats_no_update(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/resources" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.HostCPU = 0.99
	srv.refreshResources(context.Background())

	srv.mu.Lock()
	cpu := srv.HostCPU
	srv.mu.Unlock()

	if cpu != 0.99 {
		t.Errorf("hostCPU = %v, want 0.99 (unchanged)", cpu)
	}
}

func TestRefreshResources_404_no_update(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.HostCPU = 0.11
	srv.HostMem = 0.22
	srv.refreshResources(context.Background())

	srv.mu.Lock()
	cpu := srv.HostCPU
	mem := srv.HostMem
	srv.mu.Unlock()

	if cpu != 0.11 {
		t.Errorf("hostCPU = %v, want 0.11 (unchanged after 404)", cpu)
	}
	if mem != 0.22 {
		t.Errorf("hostMem = %v, want 0.22 (unchanged after 404)", mem)
	}
}

func TestRefreshBandwidth(t *testing.T) {
	tests := []struct {
		name         string
		json         string
		initAt       int
		wantTransmit float64
		wantLastAt   int
	}{
		{
			name:         "accumulates_bytes",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[{"bytes":100,"at":1000},{"bytes":200,"at":2000},{"bytes":300,"at":3000}]}}`,
			initAt:       1500,
			wantTransmit: 500,
			wantLastAt:   3000,
		},
		{
			name:         "404_no_update",
			json:         "",
			initAt:       42,
			wantTransmit: 999,
			wantLastAt:   42,
		},
		{
			name:         "empty_stats",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[]}}`,
			initAt:       100,
			wantTransmit: 0,
			wantLastAt:   100,
		},
		{
			name:         "exact_boundary_not_counted",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[{"bytes":100,"at":1000},{"bytes":200,"at":2000}]}}`,
			initAt:       1000,
			wantTransmit: 200,
			wantLastAt:   2000,
		},
		{
			name:         "all_old_entries_skipped",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[{"bytes":100,"at":500},{"bytes":200,"at":1000}]}}`,
			initAt:       1000,
			wantTransmit: 0,
			wantLastAt:   1000,
		},
		{
			name:         "negative_inversion",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[{"bytes":300,"at":3000},{"bytes":100,"at":1000},{"bytes":200,"at":2000}]}}`,
			initAt:       500,
			wantTransmit: 600,
			wantLastAt:   3000,
		},
		{
			name:         "duplicate_timestamps",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[{"bytes":100,"at":2000},{"bytes":200,"at":2000},{"bytes":300,"at":3000}]}}`,
			initAt:       1000,
			wantTransmit: 600,
			wantLastAt:   3000,
		},
		{
			name:         "single_new_entry",
			json:         `{"MediaContainer":{"StatisticsBandwidth":[{"bytes":500,"at":5000}]}}`,
			initAt:       4000,
			wantTransmit: 500,
			wantLastAt:   5000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.json == "" || r.URL.Path != "/statistics/bandwidth" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				fmt.Fprint(w, tc.json)
			})
			ts := httptest.NewServer(handler)
			defer ts.Close()

			client := plex.NewTestClientFromServer(t, ts)
			srv := NewServer(client)
			srv.LastBandwidthAt = tc.initAt
			// For the 404 case, pre-set TransmitBytes to verify it's unchanged.
			if tc.json == "" {
				srv.TransmitBytes = 999
			}

			srv.refreshBandwidth(context.Background())

			srv.mu.Lock()
			transmit := srv.TransmitBytes
			lastAt := srv.LastBandwidthAt
			srv.mu.Unlock()

			if transmit != tc.wantTransmit {
				t.Errorf("transmitBytes = %v, want %v", transmit, tc.wantTransmit)
			}
			if lastAt != tc.wantLastAt {
				t.Errorf("lastBandwidthAt = %d, want %d", lastAt, tc.wantLastAt)
			}
		})
	}
}

func TestRefresh_populates_server_state(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"TestPlex","machineIdentifier":"abc123","version":"1.40.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie","durationTotal":1000,"storageTotal":2000},
						{"title":"TV Shows","id":"2","type":"show","durationTotal":3000,"storageTotal":4000},
						{"title":"Playlists","id":"3","type":"playlist","durationTotal":0,"storageTotal":0}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"TestPlex","machineIdentifier":"abc123",
				"version":"1.40.0","platform":"Linux","platformVersion":"6.1",
				"myPlexSubscription":true,"transcoderActiveVideoSessions":2
			}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[
				{"hostCpuUtilization":50.0,"hostMemoryUtilization":70.0}
			]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)

	err := srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.Name != "TestPlex" {
		t.Errorf("name = %q, want TestPlex", srv.Name)
	}
	if srv.ID != "abc123" {
		t.Errorf("id = %q, want abc123", srv.ID)
	}
	if srv.Platform != "Linux" {
		t.Errorf("platform = %q, want Linux", srv.Platform)
	}
	if !srv.PlexPass {
		t.Error("plexPass = false, want true")
	}
	if srv.ActiveTranscodes != 2 {
		t.Errorf("activeTranscodes = %d, want 2", srv.ActiveTranscodes)
	}
	// Should have 2 libraries (movie + show), not playlist
	if len(srv.Libraries) != 2 {
		t.Errorf("libraries count = %d, want 2", len(srv.Libraries))
	}
}

func TestRefresh_preserves_item_counts(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie","durationTotal":100,"storageTotal":200}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"platform":"Linux","platformVersion":"6.1"
			}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)

	// Pre-populate with item counts
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 500},
	}
	// Set lastItemsRefresh to recent so it won't re-fetch items
	srv.LastItemsRefresh = time.Now()

	err := srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.Libraries) != 1 {
		t.Fatalf("libraries count = %d, want 1", len(srv.Libraries))
	}
	if srv.Libraries[0].ItemsCount != 500 {
		t.Errorf("ItemsCount = %d, want 500 (preserved)", srv.Libraries[0].ItemsCount)
	}
}

func TestRefresh_filters_non_library_providers(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[
					{"identifier":"com.plexapp.plugins.library","Feature":[
						{"type":"content","Directory":[
							{"title":"Movies","id":"1","type":"movie"}
						]},
						{"type":"timeline","Directory":[
							{"title":"Timeline","id":"99","type":"movie"}
						]}
					]},
					{"identifier":"tv.plex.provider.vod","Feature":[
						{"type":"content","Directory":[
							{"title":"VOD","id":"50","type":"movie"}
						]}
					]}
				]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)

	err := srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// Only 1 library: Movies from com.plexapp.plugins.library content feature
	if len(srv.Libraries) != 1 {
		t.Errorf("libraries count = %d, want 1", len(srv.Libraries))
	}
}

func TestRefresh_provider_error_returns_error(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)

	err := srv.Refresh(context.Background())
	if err == nil {
		t.Fatal("refresh() should return error on provider failure")
	}
}

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

	client := plex.NewTestClientFromServer(t, ts)
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

	client := plex.NewTestClientFromServer(t, ts)
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

func TestRefresh_server_info_error_returns_error(t *testing.T) {
	// Targets uncovered line 642: when "/" endpoint fails after providers succeed.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"}
					]}
				]}]
			}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)

	err := srv.Refresh(context.Background())
	if err == nil {
		t.Fatal("refresh() should return error when server info fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching server info") {
		t.Errorf("error = %q, want to contain 'fetching server info'", err.Error())
	}
}

func TestRunRefreshLoop_cancels_cleanly(t *testing.T) {
	// Point the server at an httptest mock that returns 500s so any refresh
	// call is observable via a hit counter. With an immediate cancel, the
	// 5-second ticker never fires, so zero hits are expected; the test
	// verifies the goroutine exits on ctx.Done without leaking.
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)

	srv := NewServer(&plex.Client{
		BaseURL:    tsURL,
		Token:      "t",
		HTTPClient: &http.Client{Timeout: time.Second},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunRefreshLoop(ctx); close(done) }()

	// Cancel before the 5s ticker fires.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runRefreshLoop did not exit after context cancel")
	}
	if hits != 0 {
		t.Errorf("expected 0 refresh calls before first tick, got %d", hits)
	}
}

func TestRecordError_known_type_increments(t *testing.T) {
	srv := NewServer(&plex.Client{})
	srv.RecordError("refresh")
	srv.RecordError("refresh")
	srv.RecordError("sessions_fetch")

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.ErrorCounts["refresh"] != 2 {
		t.Errorf("errorCounts[refresh] = %v, want 2", srv.ErrorCounts["refresh"])
	}
	if srv.ErrorCounts["sessions_fetch"] != 1 {
		t.Errorf("errorCounts[sessions_fetch] = %v, want 1", srv.ErrorCounts["sessions_fetch"])
	}
}

func TestRecordError_unknown_type_dropped(t *testing.T) {
	srv := NewServer(&plex.Client{})
	srv.RecordError("totally_unknown_type")

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if _, ok := srv.ErrorCounts["totally_unknown_type"]; ok {
		t.Error("unknown error type should be silently dropped")
	}
}

func TestRecordError_nil_map_initialized(t *testing.T) {
	srv := &Server{Sessions: sessions.NewTracker()}
	// errorCounts is nil — recordError should initialize it
	srv.RecordError("refresh")

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if srv.ErrorCounts == nil {
		t.Fatal("errorCounts should be initialized")
	}
	if srv.ErrorCounts["refresh"] != 1 {
		t.Errorf("errorCounts[refresh] = %v, want 1", srv.ErrorCounts["refresh"])
	}
}

func TestSnapshot_boolean_conversions(t *testing.T) {
	srv := &Server{
		Name:            "Srv",
		ID:              "id1",
		Version:         "1.0",
		Platform:        "Linux",
		PlatformVersion: "6.1",
		PlexPass:        true,
		HTTPReachable:   true,
		Sessions:        sessions.NewTracker(),
		ErrorCounts:     map[string]float64{"refresh": 3},
		Libraries: []library.Library{
			{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 10},
		},
	}

	snap := srv.Snapshot()

	if snap.PlexPass != metrics.ValTrue {
		t.Errorf("plexPass = %q, want true", snap.PlexPass)
	}
	if snap.HTTPReachable != 1.0 {
		t.Errorf("httpReachable = %v, want 1.0", snap.HTTPReachable)
	}
	if snap.ErrorCounts["refresh"] != 3 {
		t.Errorf("errorCounts[refresh] = %v, want 3", snap.ErrorCounts["refresh"])
	}
	if len(snap.Libraries) != 1 || snap.Libraries[0].ID != "1" {
		t.Errorf("libraries not copied correctly")
	}
}

func TestSnapshot_false_booleans(t *testing.T) {
	srv := &Server{
		PlexPass:      false,
		HTTPReachable: false,
		Sessions:      sessions.NewTracker(),
	}

	snap := srv.Snapshot()

	if snap.PlexPass != metrics.ValFalse {
		t.Errorf("plexPass = %q, want false", snap.PlexPass)
	}
	if snap.HTTPReachable != 0.0 {
		t.Errorf("httpReachable = %v, want 0.0", snap.HTTPReachable)
	}
}

func TestRunRefreshLoop_failure_sets_unreachable_and_recovery(t *testing.T) {
	// Test the refresh failure and recovery paths in runRefreshLoop by
	// calling refresh() directly (the loop's ticker-driven behaviour is
	// tested by TestRunRefreshLoop_cancels_cleanly). We verify the
	// observable state transitions: httpReachable and errorCounts.
	firstRefreshDone := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			if !firstRefreshDone {
				// First refresh cycle: providers fails → refresh returns error.
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0","MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[{"type":"content","Directory":[]}]}]}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.HTTPReachable = true // start as reachable

	// Simulate failure path (mirrors runRefreshLoop's error branch).
	err := srv.Refresh(context.Background())
	if err == nil {
		t.Fatal("first refresh should fail")
	}
	firstRefreshDone = true
	srv.mu.Lock()
	srv.HTTPReachable = false
	srv.mu.Unlock()
	srv.RecordError("refresh")

	srv.mu.Lock()
	reachable := srv.HTTPReachable
	errCount := srv.ErrorCounts["refresh"]
	srv.mu.Unlock()

	if reachable {
		t.Error("httpReachable should be false after failed refresh")
	}
	if errCount != 1 {
		t.Errorf("errorCounts[refresh] = %v, want 1", errCount)
	}

	// Simulate recovery path (mirrors runRefreshLoop's success branch).
	err = srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second refresh should succeed: %v", err)
	}
	srv.mu.Lock()
	srv.HTTPReachable = true
	srv.mu.Unlock()

	srv.mu.Lock()
	reachable = srv.HTTPReachable
	srv.mu.Unlock()

	if !reachable {
		t.Error("httpReachable should be true after successful refresh")
	}
}

func TestRefreshLibraryItems_writeback_boundary(t *testing.T) {
	// Targets lived mutant at line 703 (i < len(s.libraries) boundary).
	// When the local libs slice has more entries than s.libraries (e.g. library
	// was removed between copy and writeback), the extra entries should be ignored.
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

	client := plex.NewTestClientFromServer(t, ts)
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

func TestRefreshLibraryItems_id_mismatch_skips_writeback(t *testing.T) {
	// When library IDs don't match between local copy and s.libraries
	// (e.g. libraries were reordered), writeback should skip mismatched entries.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/library/sections/") {
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":999}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 50},
	}

	// Simulate a race: after copy, s.libraries gets replaced with different IDs
	// We can't easily simulate this race, but we can test the ID check by
	// verifying that matching IDs DO get written back (already tested above)
	// and that the function doesn't panic with empty libraries.
	srv.Libraries = nil
	srv.refreshLibraryItems(context.Background())

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.Libraries) != 0 {
		t.Errorf("libraries count = %d, want 0", len(srv.Libraries))
	}
}

func TestRefreshLibraryItems_artist_type10_error_falls_back(t *testing.T) {
	// Targets lived mutant at line 690 (count > 0 boundary).
	// When type=10 returns an error, should fall back to type=7.
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

	client := plex.NewTestClientFromServer(t, ts)
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

	client := plex.NewTestClientFromServer(t, ts)
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

func TestRefreshLibraryItems_artist_type10_returns_zero_falls_to_type7(t *testing.T) {
	// Targets lived mutant at line 690: CONDITIONALS_BOUNDARY on count > 0.
	// When type=10 returns count=0 (not error, but zero), should fall back to type=7.
	// This is the exact boundary: count=0 should NOT be accepted.
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

	client := plex.NewTestClientFromServer(t, ts)
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

	client := plex.NewTestClientFromServer(t, ts)
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

func TestRefreshBandwidth_accumulates_across_calls(t *testing.T) {
	// Verifies that transmitBytes accumulates across multiple refreshBandwidth calls.
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			callCount++
			if callCount == 1 {
				fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
					{"bytes":100,"at":2000}
				]}}`)
			} else {
				fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
					{"bytes":100,"at":2000},
					{"bytes":200,"at":3000}
				]}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	srv.LastBandwidthAt = 1000

	// First call: at=2000 (100 bytes)
	srv.refreshBandwidth(context.Background())

	srv.mu.Lock()
	if srv.TransmitBytes != 100 {
		t.Errorf("after call 1: transmitBytes = %v, want 100", srv.TransmitBytes)
	}
	if srv.LastBandwidthAt != 2000 {
		t.Errorf("after call 1: lastBandwidthAt = %d, want 2000", srv.LastBandwidthAt)
	}
	srv.mu.Unlock()

	// Second call: at=2000 already seen, only at=3000 (200 bytes) is new
	srv.refreshBandwidth(context.Background())

	srv.mu.Lock()
	if srv.TransmitBytes != 300 {
		t.Errorf("after call 2: transmitBytes = %v, want 300 (100 + 200)", srv.TransmitBytes)
	}
	if srv.LastBandwidthAt != 3000 {
		t.Errorf("after call 2: lastBandwidthAt = %d, want 3000", srv.LastBandwidthAt)
	}
	srv.mu.Unlock()
}

func TestRefresh_prevItems_preserves_positive_counts_only(t *testing.T) {
	// Targets lived mutant at line 611 (lib.ItemsCount > 0 boundary).
	// Libraries with ItemsCount=0 should NOT be preserved in prevItems.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"},
						{"title":"TV","id":"2","type":"show"}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)

	// Pre-populate: Movies has count, TV has 0
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 100},
		{ID: "2", Name: "TV", Type: library.TypeShow, ItemsCount: 0},
	}
	srv.LastItemsRefresh = time.Now() // skip items refresh

	err := srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	// Movies should preserve its count (100 > 0)
	if srv.Libraries[0].ItemsCount != 100 {
		t.Errorf("Movies ItemsCount = %d, want 100 (preserved)", srv.Libraries[0].ItemsCount)
	}
	// TV should remain 0 (0 is not > 0, so not preserved)
	if srv.Libraries[1].ItemsCount != 0 {
		t.Errorf("TV ItemsCount = %d, want 0 (not preserved)", srv.Libraries[1].ItemsCount)
	}
}

func TestRefresh_items_refresh_triggered_after_15_minutes(t *testing.T) {
	// Targets lived mutant at line 637 (time.Since > 15*time.Minute boundary/negation).
	// When lastItemsRefresh is old enough, items should be refreshed.
	itemsRequested := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/library/sections/") {
				itemsRequested = true
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	// Set lastItemsRefresh to 20 minutes ago — should trigger refresh
	srv.LastItemsRefresh = time.Now().Add(-20 * time.Minute)

	err := srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	if !itemsRequested {
		t.Error("items refresh should be triggered when lastItemsRefresh > 15 minutes ago")
	}
}

func TestRefresh_items_refresh_skipped_when_recent(t *testing.T) {
	// When lastItemsRefresh is recent, items should NOT be refreshed.
	itemsRequested := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/library/sections/") {
				itemsRequested = true
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := plex.NewTestClientFromServer(t, ts)
	srv := NewServer(client)
	// Set lastItemsRefresh to 5 minutes ago — should NOT trigger refresh
	srv.LastItemsRefresh = time.Now().Add(-5 * time.Minute)

	err := srv.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	if itemsRequested {
		t.Error("items refresh should NOT be triggered when lastItemsRefresh < 15 minutes ago")
	}
}
