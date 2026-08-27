package library

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	plexapi "github.com/cplieger/plexapi/v2"
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
		providers plexapi.MediaProviders
		prevItems map[string]int64
		wantIDs   []string
	}{
		{
			name: "filters non-library provider and non-content feature",
			providers: plexapi.MediaProviders{
				MediaProviders: []plexapi.MediaProvider{
					{
						Identifier: "com.plexapp.plugins.library",
						Features: []plexapi.ProviderFeature{
							{
								Type: "content",
								Directories: []plexapi.ProviderDirectory{
									{Title: "Movies", ID: "1", Type: "movie", DurationTotal: 100, StorageTotal: 200},
									{Title: "Playlists", ID: "2", Type: "playlist"},
								},
							},
							{
								Type: "timeline",
								Directories: []plexapi.ProviderDirectory{
									{Title: "Timeline", ID: "99", Type: "movie"},
								},
							},
						},
					},
					{
						Identifier: "tv.plex.provider.vod",
						Features: []plexapi.ProviderFeature{
							{
								Type: "content",
								Directories: []plexapi.ProviderDirectory{
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
			providers: plexapi.MediaProviders{},
			wantIDs:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(&tt.providers, tt.prevItems)
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
	providers := plexapi.MediaProviders{
		MediaProviders: []plexapi.MediaProvider{
			{
				Identifier: "com.plexapp.plugins.library",
				Features: []plexapi.ProviderFeature{
					{
						Type: "content",
						Directories: []plexapi.ProviderDirectory{
							{Title: "Movies", ID: "1", Type: "movie"},
							{Title: "TV", ID: "2", Type: "show"},
							{Title: "Music", ID: "3", Type: "artist"},
						},
					},
				},
			},
		},
	}
	// "2" carries a read zero, "3" is absent (never read), "99" has no section.
	prevItems := map[string]int64{"1": 500, "2": 0, "99": 999}
	got := Build(&providers, prevItems)
	if len(got) != 3 {
		t.Fatalf("Build() returned %d libs, want 3", len(got))
	}
	if got[0].ItemsCount != 500 || !got[0].ItemsKnown {
		t.Errorf("Movies = (%d, known %t), want (500, known true) from prevItems", got[0].ItemsCount, got[0].ItemsKnown)
	}
	if got[1].ItemsCount != 0 || !got[1].ItemsKnown {
		t.Errorf("TV = (%d, known %t), want (0, known true): a read zero is a count, not an absence",
			got[1].ItemsCount, got[1].ItemsKnown)
	}
	if got[2].ItemsKnown {
		t.Errorf("Music = (%d, known %t), want known false: absent from prevItems means never read",
			got[2].ItemsCount, got[2].ItemsKnown)
	}
}

// --- Tests: ItemCountTypes ---

func TestItemCountTypes(t *testing.T) {
	tests := []struct {
		libType string
		want    []int
	}{
		{TypeShow, []int{4}},
		{TypeArtist, []int{10, 7, 0}},
		{TypeMovie, []int{0}},
		{"photo", []int{0}},
		{"unknown", []int{0}},
	}
	for _, tt := range tests {
		t.Run(tt.libType, func(t *testing.T) {
			got := ItemCountTypes(tt.libType)
			if len(got) != len(tt.want) {
				t.Fatalf("ItemCountTypes(%q) = %v, want %v", tt.libType, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ItemCountTypes(%q)[%d] = %d, want %d", tt.libType, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuild_skips_non_numeric_section_id(t *testing.T) {
	providers := plexapi.MediaProviders{
		MediaProviders: []plexapi.MediaProvider{
			{
				Identifier: "com.plexapp.plugins.library",
				Features: []plexapi.ProviderFeature{
					{
						Type: "content",
						Directories: []plexapi.ProviderDirectory{
							{Title: "Movies", ID: "1", Type: "movie"},
							{Title: "Injected", ID: "1/all?x=../../etc", Type: "movie"},
							{Title: "Empty", ID: "", Type: "movie"},
						},
					},
				},
			},
		},
	}
	got := Build(&providers, nil)
	if len(got) != 1 {
		t.Fatalf("Build emitted %d libraries, want 1 (non-numeric section IDs must be skipped before URL interpolation)", len(got))
	}
	if got[0].ID != "1" {
		t.Errorf("kept library ID = %q, want the numeric id 1", got[0].ID)
	}
}

func buildNMovieSections(t *testing.T, n int) plexapi.MediaProviders {
	t.Helper()
	dirs := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		dirs = append(dirs, fmt.Sprintf(`{"title":"lib%d","id":"%d","type":"movie"}`, i, i))
	}
	js := fmt.Sprintf(`{"MediaProvider":[{"identifier":%q,"Feature":[{"type":%q,"Directory":[%s]}]}]}`,
		PluginIdentifier, FeatureContent, strings.Join(dirs, ","))
	var providers plexapi.MediaProviders
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
			p := buildNMovieSections(t, tt.sections)
			got := Build(&p, nil)
			if len(got) != tt.want {
				t.Errorf("Build with %d numeric sections returned %d libraries, want %d (MaxLibraries cap)",
					tt.sections, len(got), tt.want)
			}
		})
	}
}
