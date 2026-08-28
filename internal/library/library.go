package library

import (
	"strconv"

	"github.com/cplieger/plexapi/v2"
)

// Library types recognised by the exporter. Values match the Plex
// server's "type" field on a library directory; they are part of the
// frozen wire/log/metric contract and must not change.
const (
	TypeMovie     = "movie"
	TypeShow      = "show"
	TypeArtist    = "artist"
	TypePhoto     = "photo"
	TypeHomevideo = "homevideo"
)

// Session media types observed in Plex playback notifications.
const (
	TypeEpisode = "episode"
	TypeTrack   = "track"
)

// Plex API identifiers and feature types.
const (
	PluginIdentifier = "com.plexapp.plugins.library"
	FeatureContent   = "content"
	CountLabelItems  = "items"
)

// MaxLibraries bounds the number of library sections the exporter tracks and
// emits metrics for. Like sessions.MaxTrackedSessions, it caps Prometheus label
// cardinality (one series set per library) against a compromised or buggy Plex
// server returning an unbounded list of distinct numeric section IDs in
// /media/providers.
const MaxLibraries = 256

// Library is a Plex library (section) entry. Fields are exported
// because consumers elsewhere in the exporter read them directly.
//
// ItemsCount is only meaningful when ItemsKnown is set: a library emptied to
// exactly zero items and a library whose count was never read both hold 0, and
// only the first may be published. The zero value is therefore "never read",
// which is the correct state for a freshly built entry.
type Library struct {
	ID, Name, Type string
	DurationTotal  int64
	StorageTotal   int64
	ItemsCount     int64
	ItemsKnown     bool
}

// IsType reports whether t is a library type the exporter emits
// metrics for.
func IsType(t string) bool {
	switch t {
	case TypeMovie, TypeShow, TypeArtist, TypePhoto, TypeHomevideo:
		return true
	}
	return false
}

// ContentTypeLabel returns the plural noun used as the
// "content_type" Prometheus label for libraries of libType.
func ContentTypeLabel(libType string) string {
	switch libType {
	case TypeMovie:
		return "movies"
	case TypeShow:
		return "episodes"
	case TypeArtist:
		return "tracks"
	case TypePhoto:
		return "photos"
	default:
		return CountLabelItems
	}
}

// isCountableSection reports whether a media-provider directory is a library
// section the exporter emits metrics for: a recognised type with a numeric
// section id. The numeric check matters because the id is later interpolated
// into /library/sections/<id>/all, so a non-numeric id must be rejected before
// URL construction.
func isCountableSection(libType, id string) bool {
	if !IsType(libType) {
		return false
	}
	_, err := strconv.Atoi(id)
	return err == nil
}

// Build extracts library entries from the media providers response,
// preserving existing item counts from prevItems. A section absent from
// prevItems has never had its count read, so it comes back with ItemsKnown
// false rather than a count of zero.
func Build(providers *plexapi.MediaProviders, prevItems map[string]int64) []Library {
	var libs []Library
	for _, p := range providers.MediaProviders {
		if p.Identifier != PluginIdentifier {
			continue
		}
		for _, f := range p.Features {
			if f.Type != FeatureContent {
				continue
			}
			for _, d := range f.Directories {
				count, known := prevItems[d.ID]
				libs = appendLibrary(libs, &Library{
					ID: d.ID, Name: d.Title, Type: d.Type,
					DurationTotal: d.DurationTotal, StorageTotal: d.StorageTotal,
					ItemsCount: count, ItemsKnown: known,
				})
			}
		}
	}
	return libs
}

// appendLibrary appends lib to libs when lib is a countable section and the
// MaxLibraries cap has not yet been reached, returning the (possibly
// unchanged) slice. Centralising the countable + cardinality-cap checks keeps
// Build's nesting shallow and the cap in one place. Building a candidate
// Library for a non-countable directory is harmless: the value is discarded.
func appendLibrary(libs []Library, lib *Library) []Library {
	if !isCountableSection(lib.Type, lib.ID) {
		return libs
	}
	if len(libs) >= MaxLibraries {
		return libs
	}
	return append(libs, *lib)
}

// ItemCountTypes returns the Plex metadata-type IDs to try in order for
// the given library type's item count. 0 means no type filter (the
// default path), matching plexapi.CountSectionItems's contract.
func ItemCountTypes(libType string) []int {
	switch libType {
	case TypeShow:
		return []int{4} // episodes
	case TypeArtist:
		return []int{10, 7, 0} // tracks, fallback, default
	default:
		return []int{0}
	}
}
