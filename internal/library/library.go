package library

import "github.com/cplieger/plex-exporter/internal/plexapi"

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
	TypeClip    = "clip"
)

// Plex API identifiers and feature types.
const (
	PluginIdentifier = "com.plexapp.plugins.library"
	FeatureContent   = "content"
	CountLabelItems  = "items"
)

// KnownSessionMediaTypes is the set of media types valid as Prometheus
// label values for session metrics. Unknown values are normalised to
// "other" by the collector.
var KnownSessionMediaTypes = map[string]bool{
	TypeMovie: true, TypeEpisode: true, TypeTrack: true, TypeClip: true, TypePhoto: true,
}

// Library is a Plex library (section) entry. Fields are exported
// because consumers elsewhere in the exporter read them directly.
type Library struct {
	ID, Name, Type string
	DurationTotal  int64
	StorageTotal   int64
	ItemsCount     int64
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

// Build extracts library entries from the media providers response,
// preserving existing item counts from prevItems.
func Build(providers plexapi.MediaProviderResponse, prevItems map[string]int64) []Library {
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
				if !IsType(d.Type) {
					continue
				}
				libs = append(libs, Library{
					ID: d.ID, Name: d.Title, Type: d.Type,
					DurationTotal: d.DurationTotal, StorageTotal: d.StorageTotal,
					ItemsCount: prevItems[d.ID],
				})
			}
		}
	}
	return libs
}

// ItemCountTypes returns the `type=` query params to try in order for
// the given library type. Empty string means no filter (default path).
func ItemCountTypes(libType string) []string {
	switch libType {
	case TypeShow:
		return []string{"4"} // episodes
	case TypeArtist:
		return []string{"10", "7", ""} // tracks, fallback, default
	default:
		return []string{""}
	}
}
