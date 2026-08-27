package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/plex-exporter/v2/internal/library"
	"github.com/cplieger/plex-exporter/v2/internal/metrics"
	"github.com/cplieger/plex-exporter/v2/internal/plex"
	"github.com/cplieger/plex-exporter/v2/internal/plextest"
	"github.com/cplieger/plex-exporter/v2/internal/sessions"
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.refreshResources(t.Context())

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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.HostCPU = 0.99
	srv.refreshResources(t.Context())

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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.HostCPU = 0.11
	srv.HostMem = 0.22
	srv.refreshResources(t.Context())

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

			client := plextest.NewTestClientFromServer(t, ts)
			srv := New(client)
			srv.LastBandwidthAt = tc.initAt
			// For the 404 case, pre-set TransmitBytes to verify it's unchanged.
			if tc.json == "" {
				srv.TransmitBytes = 999
			}

			srv.refreshBandwidth(t.Context())

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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	err := srv.Refresh(t.Context())
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	// Pre-populate with item counts
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 500, ItemsKnown: true},
	}
	// Set lastItemsRefresh to recent so it won't re-fetch items
	srv.LastItemsRefresh = time.Now()

	err := srv.Refresh(t.Context())
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	err := srv.Refresh(t.Context())
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	err := srv.Refresh(t.Context())
	if err == nil {
		t.Fatal("refresh() should return error on provider failure")
	}
}

func TestRefresh_server_info_error_returns_error(t *testing.T) {
	// When the "/" server-info fetch fails after providers succeed, Refresh
	// must return the wrapped error.
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	err := srv.Refresh(t.Context())
	if err == nil {
		t.Fatal("refresh() should return error when server info fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching server info") {
		t.Errorf("error = %q, want to contain 'fetching server info'", err.Error())
	}
}

func TestRunRefreshLoop_cancels_cleanly(t *testing.T) {
	// Bubbled, which turns the negative timer assertion into a proof. The
	// synthetic clock only advances when every bubble goroutine is durably
	// blocked, so cancel() lands before the loop's 5s ticker can fire and
	// "zero refresh calls" is exact rather than a race the real clock usually
	// won. httptest.NewTestServer is what makes the bubble legal: it serves
	// over an in-memory net, where httptest.NewServer's accept goroutine
	// parks indefinitely on a real socket, never becomes durably blocked, and
	// so would freeze the bubble's clock. hits is atomic because the handler
	// writes it from a different goroutine than the assertion reads it.
	synctest.Test(t, func(t *testing.T) {
		var hits atomic.Int64
		ts := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))

		// NewTestServer leaves Server.URL unset and its client routes every
		// request to the handler regardless of host, so this base URL is a
		// placeholder that is never dialed -- it only has to satisfy
		// plexapi's scheme+host check, and a loopback host keeps the
		// library's plaintext-to-a-remote-host warning out of test output.
		client, err := plex.NewClientFromHTTP("http://127.0.0.1:32400", "t", ts.Client())
		if err != nil {
			t.Fatal(err)
		}
		srv := New(client)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { srv.RunRefreshLoop(ctx); close(done) }()

		// Cancel before the 5s ticker fires.
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runRefreshLoop did not exit after context cancel")
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("expected 0 refresh calls before first tick, got %d", n)
		}
	})
}

func TestRecordError_known_type_increments(t *testing.T) {
	srv := New(&plex.Client{})
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
	srv := New(&plex.Client{})
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
		Name:              "Srv",
		ID:                "id1",
		Version:           "1.0",
		Platform:          "Linux",
		PlatformVersion:   "6.1",
		PlexPass:          true,
		HTTPReachable:     true,
		SessionsReachable: true,
		Sessions:          sessions.NewTracker(),
		ErrorCounts:       map[string]float64{"refresh": 3},
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
	if snap.SessionsReachable != 1.0 {
		t.Errorf("sessionsReachable = %v, want 1.0", snap.SessionsReachable)
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
		PlexPass:          false,
		HTTPReachable:     false,
		SessionsReachable: false,
		Sessions:          sessions.NewTracker(),
	}

	snap := srv.Snapshot()

	if snap.PlexPass != metrics.ValFalse {
		t.Errorf("plexPass = %q, want false", snap.PlexPass)
	}
	if snap.HTTPReachable != 0.0 {
		t.Errorf("httpReachable = %v, want 0.0", snap.HTTPReachable)
	}
	if snap.SessionsReachable != 0.0 {
		t.Errorf("sessionsReachable = %v, want 0.0", snap.SessionsReachable)
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	srv.LastBandwidthAt = 1000

	// First call: at=2000 (100 bytes)
	srv.refreshBandwidth(t.Context())

	srv.mu.Lock()
	if srv.TransmitBytes != 100 {
		t.Errorf("after call 1: transmitBytes = %v, want 100", srv.TransmitBytes)
	}
	if srv.LastBandwidthAt != 2000 {
		t.Errorf("after call 1: lastBandwidthAt = %d, want 2000", srv.LastBandwidthAt)
	}
	srv.mu.Unlock()

	// Second call: at=2000 already seen, only at=3000 (200 bytes) is new
	srv.refreshBandwidth(t.Context())

	srv.mu.Lock()
	if srv.TransmitBytes != 300 {
		t.Errorf("after call 2: transmitBytes = %v, want 300 (100 + 200)", srv.TransmitBytes)
	}
	if srv.LastBandwidthAt != 3000 {
		t.Errorf("after call 2: lastBandwidthAt = %d, want 3000", srv.LastBandwidthAt)
	}
	srv.mu.Unlock()
}

func TestRefresh_prevItems_preserves_known_counts_only(t *testing.T) {
	// A count that was read carries across a library-list rebuild, a zero one
	// included; a count that was never read stays unread rather than being
	// rebuilt as a published zero.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"},
						{"title":"TV","id":"2","type":"show"},
						{"title":"Music","id":"3","type":"artist"}
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)

	// Pre-populate one of each state: a read positive count, a read zero, and
	// a section whose count has never been read.
	srv.Libraries = []library.Library{
		{ID: "1", Name: "Movies", Type: library.TypeMovie, ItemsCount: 100, ItemsKnown: true},
		{ID: "2", Name: "TV", Type: library.TypeShow, ItemsCount: 0, ItemsKnown: true},
		{ID: "3", Name: "Music", Type: library.TypeArtist},
	}
	srv.LastItemsRefresh = time.Now() // skip items refresh

	err := srv.Refresh(t.Context())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.Libraries) != 3 {
		t.Fatalf("libraries count = %d, want 3", len(srv.Libraries))
	}
	if got := srv.Libraries[0]; got.ItemsCount != 100 || !got.ItemsKnown {
		t.Errorf("Movies = (%d, known %t), want (100, known true)", got.ItemsCount, got.ItemsKnown)
	}
	if got := srv.Libraries[1]; got.ItemsCount != 0 || !got.ItemsKnown {
		t.Errorf("TV = (%d, known %t), want (0, known true) — a read zero must survive the rebuild",
			got.ItemsCount, got.ItemsKnown)
	}
	if got := srv.Libraries[2]; got.ItemsKnown {
		t.Errorf("Music = (%d, known %t), want known false for a count never read",
			got.ItemsCount, got.ItemsKnown)
	}
}
func TestRefresh_items_refresh_triggered_after_15_minutes(t *testing.T) {
	// When lastItemsRefresh is older than 15 minutes, item counts are refetched.
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	// Set lastItemsRefresh to 20 minutes ago — should trigger refresh
	srv.LastItemsRefresh = time.Now().Add(-20 * time.Minute)

	err := srv.Refresh(t.Context())
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

	client := plextest.NewTestClientFromServer(t, ts)
	srv := New(client)
	// Set lastItemsRefresh to 5 minutes ago — should NOT trigger refresh
	srv.LastItemsRefresh = time.Now().Add(-5 * time.Minute)

	err := srv.Refresh(t.Context())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	if itemsRequested {
		t.Error("items refresh should NOT be triggered when lastItemsRefresh < 15 minutes ago")
	}
}
