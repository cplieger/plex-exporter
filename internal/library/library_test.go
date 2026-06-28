package library

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cplieger/plex-exporter/internal/plexapi"
	"pgregory.net/rapid"
)

func TestContentTypeLabel(t *testing.T) {
	tests := []struct {
		libType string
		want    string
	}{
		{TypeMovie, "movies"},
		{TypeShow, "episodes"},
		{TypeArtist, "tracks"},
		{"photo", "photos"},
		{"homevideo", "items"},
		{"other", "items"},
	}
	for _, tt := range tests {
		t.Run(tt.libType, func(t *testing.T) {
			if got := ContentTypeLabel(tt.libType); got != tt.want {
				t.Errorf("ContentTypeLabel(%q) = %q, want %q", tt.libType, got, tt.want)
			}
		})
	}
}

func TestIsLibraryType(t *testing.T) {
	valid := []string{"movie", "show", "artist", "photo", "homevideo"}
	for _, v := range valid {
		if !IsType(v) {
			t.Errorf("IsType(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "clip", "playlist", "other"}
	for _, v := range invalid {
		if IsType(v) {
			t.Errorf("IsType(%q) = true, want false", v)
		}
	}
}

func TestIsLibraryType_random_strings_mostly_false(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "type")
		got := IsType(s)
		valid := map[string]bool{"movie": true, "show": true, "artist": true, "photo": true, "homevideo": true}
		if got != valid[s] {
			t.Errorf("IsType(%q) = %v, want %v", s, got, valid[s])
		}
	})
}

func TestContentTypeLabel_always_returns_non_empty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "libType")
		got := ContentTypeLabel(s)
		if got == "" {
			t.Errorf("ContentTypeLabel(%q) returned empty string", s)
		}
	})
}

// --- Tests: Build ---

func TestBuild(t *testing.T) {
	tests := []struct {
		name      string
		providers plexapi.MediaProviderResponse
		prevItems map[string]int64
		wantIDs   []string
	}{
		{
			name: "filters non-library provider and non-content feature",
			providers: plexapi.MediaProviderResponse{
				MediaProviders: []struct {
					Identifier string `json:"identifier"`
					Features   []struct {
						Type        string `json:"type"`
						Directories []struct {
							Title         string `json:"title"`
							ID            string `json:"id"`
							Type          string `json:"type"`
							DurationTotal int64  `json:"durationTotal"`
							StorageTotal  int64  `json:"storageTotal"`
						} `json:"Directory"`
					} `json:"Feature"`
				}{
					{
						Identifier: "com.plexapp.plugins.library",
						Features: []struct {
							Type        string `json:"type"`
							Directories []struct {
								Title         string `json:"title"`
								ID            string `json:"id"`
								Type          string `json:"type"`
								DurationTotal int64  `json:"durationTotal"`
								StorageTotal  int64  `json:"storageTotal"`
							} `json:"Directory"`
						}{
							{
								Type: "content",
								Directories: []struct {
									Title         string `json:"title"`
									ID            string `json:"id"`
									Type          string `json:"type"`
									DurationTotal int64  `json:"durationTotal"`
									StorageTotal  int64  `json:"storageTotal"`
								}{
									{Title: "Movies", ID: "1", Type: "movie", DurationTotal: 100, StorageTotal: 200},
									{Title: "Playlists", ID: "2", Type: "playlist"},
								},
							},
							{
								Type: "timeline",
								Directories: []struct {
									Title         string `json:"title"`
									ID            string `json:"id"`
									Type          string `json:"type"`
									DurationTotal int64  `json:"durationTotal"`
									StorageTotal  int64  `json:"storageTotal"`
								}{
									{Title: "Timeline", ID: "99", Type: "movie"},
								},
							},
						},
					},
					{
						Identifier: "tv.plex.provider.vod",
						Features: []struct {
							Type        string `json:"type"`
							Directories []struct {
								Title         string `json:"title"`
								ID            string `json:"id"`
								Type          string `json:"type"`
								DurationTotal int64  `json:"durationTotal"`
								StorageTotal  int64  `json:"storageTotal"`
							} `json:"Directory"`
						}{
							{
								Type: "content",
								Directories: []struct {
									Title         string `json:"title"`
									ID            string `json:"id"`
									Type          string `json:"type"`
									DurationTotal int64  `json:"durationTotal"`
									StorageTotal  int64  `json:"storageTotal"`
								}{
									{Title: "VOD", ID: "50", Type: "movie"},
								},
							},
						},
					},
				},
			},
			wantIDs: []string{"1"},
		},
		{
			name:      "empty providers returns nil",
			providers: plexapi.MediaProviderResponse{},
			wantIDs:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(tt.providers, tt.prevItems)
			var gotIDs []string
			for _, lb := range got {
				gotIDs = append(gotIDs, lb.ID)
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("Build() returned %d libs %v, want %d %v", len(gotIDs), gotIDs, len(tt.wantIDs), tt.wantIDs)
			}
			for i := range gotIDs {
				if gotIDs[i] != tt.wantIDs[i] {
					t.Errorf("lib[%d].ID = %q, want %q", i, gotIDs[i], tt.wantIDs[i])
				}
			}
		})
	}
}

func TestBuild_prevItems_preserved(t *testing.T) {
	providers := plexapi.MediaProviderResponse{
		MediaProviders: []struct {
			Identifier string `json:"identifier"`
			Features   []struct {
				Type        string `json:"type"`
				Directories []struct {
					Title         string `json:"title"`
					ID            string `json:"id"`
					Type          string `json:"type"`
					DurationTotal int64  `json:"durationTotal"`
					StorageTotal  int64  `json:"storageTotal"`
				} `json:"Directory"`
			} `json:"Feature"`
		}{
			{
				Identifier: "com.plexapp.plugins.library",
				Features: []struct {
					Type        string `json:"type"`
					Directories []struct {
						Title         string `json:"title"`
						ID            string `json:"id"`
						Type          string `json:"type"`
						DurationTotal int64  `json:"durationTotal"`
						StorageTotal  int64  `json:"storageTotal"`
					} `json:"Directory"`
				}{
					{
						Type: "content",
						Directories: []struct {
							Title         string `json:"title"`
							ID            string `json:"id"`
							Type          string `json:"type"`
							DurationTotal int64  `json:"durationTotal"`
							StorageTotal  int64  `json:"storageTotal"`
						}{
							{Title: "Movies", ID: "1", Type: "movie"},
						},
					},
				},
			},
		},
	}
	prevItems := map[string]int64{"1": 500, "99": 999}
	got := Build(providers, prevItems)
	if len(got) != 1 {
		t.Fatalf("Build() returned %d libs, want 1", len(got))
	}
	if got[0].ItemsCount != 500 {
		t.Errorf("ItemsCount = %d, want 500 (from prevItems)", got[0].ItemsCount)
	}
}

// --- Tests: ItemCountTypes ---

func TestItemCountTypes(t *testing.T) {
	tests := []struct {
		libType string
		want    []string
	}{
		{TypeShow, []string{"4"}},
		{TypeArtist, []string{"10", "7", ""}},
		{TypeMovie, []string{""}},
		{"photo", []string{""}},
		{"unknown", []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.libType, func(t *testing.T) {
			got := ItemCountTypes(tt.libType)
			if len(got) != len(tt.want) {
				t.Fatalf("ItemCountTypes(%q) = %v, want %v", tt.libType, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ItemCountTypes(%q)[%d] = %q, want %q", tt.libType, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuild_skips_non_numeric_section_id(t *testing.T) {
	providers := plexapi.MediaProviderResponse{
		MediaProviders: []struct {
			Identifier string `json:"identifier"`
			Features   []struct {
				Type        string `json:"type"`
				Directories []struct {
					Title         string `json:"title"`
					ID            string `json:"id"`
					Type          string `json:"type"`
					DurationTotal int64  `json:"durationTotal"`
					StorageTotal  int64  `json:"storageTotal"`
				} `json:"Directory"`
			} `json:"Feature"`
		}{
			{
				Identifier: "com.plexapp.plugins.library",
				Features: []struct {
					Type        string `json:"type"`
					Directories []struct {
						Title         string `json:"title"`
						ID            string `json:"id"`
						Type          string `json:"type"`
						DurationTotal int64  `json:"durationTotal"`
						StorageTotal  int64  `json:"storageTotal"`
					} `json:"Directory"`
				}{
					{
						Type: "content",
						Directories: []struct {
							Title         string `json:"title"`
							ID            string `json:"id"`
							Type          string `json:"type"`
							DurationTotal int64  `json:"durationTotal"`
							StorageTotal  int64  `json:"storageTotal"`
						}{
							{Title: "Movies", ID: "1", Type: "movie"},
							{Title: "Injected", ID: "1/all?x=../../etc", Type: "movie"},
							{Title: "Empty", ID: "", Type: "movie"},
						},
					},
				},
			},
		},
	}
	got := Build(providers, nil)
	if len(got) != 1 {
		t.Fatalf("Build emitted %d libraries, want 1 (non-numeric section IDs must be skipped before URL interpolation)", len(got))
	}
	if got[0].ID != "1" {
		t.Errorf("kept library ID = %q, want the numeric id 1", got[0].ID)
	}
}

func buildNMovieSections(t *testing.T, n int) plexapi.MediaProviderResponse {
	t.Helper()
	dirs := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		dirs = append(dirs, fmt.Sprintf(`{"title":"lib%d","id":"%d","type":"movie"}`, i, i))
	}
	js := fmt.Sprintf(`{"MediaProvider":[{"identifier":%q,"Feature":[{"type":%q,"Directory":[%s]}]}]}`,
		PluginIdentifier, FeatureContent, strings.Join(dirs, ","))
	var providers plexapi.MediaProviderResponse
	if err := json.Unmarshal([]byte(js), &providers); err != nil {
		t.Fatalf("buildNMovieSections: %v", err)
	}
	return providers
}

func TestBuild_caps_library_count_at_MaxLibraries(t *testing.T) {
	tests := []struct {
		name     string
		sections int
		want     int
	}{
		{name: "under cap kept in full", sections: 10, want: 10},
		{name: "exactly at cap kept in full", sections: MaxLibraries, want: MaxLibraries},
		{name: "one over cap drops the extra", sections: MaxLibraries + 1, want: MaxLibraries},
		{name: "far over cap clamps to cap", sections: MaxLibraries + 100, want: MaxLibraries},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(buildNMovieSections(t, tt.sections), nil)
			if len(got) != tt.want {
				t.Errorf("Build with %d numeric sections returned %d libraries, want %d (MaxLibraries cap)",
					tt.sections, len(got), tt.want)
			}
		})
	}
}
