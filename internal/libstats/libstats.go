// Package libstats turns one walk of a Plex library's items into the
// aggregate figures the exporter publishes: sizes by resolution, codec and
// last play, the largest items, and the newest arrivals. It does no I/O and
// logs nothing.
package libstats

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/cplieger/plex-exporter/internal/metrics"
	"github.com/cplieger/plexapi/v2"
)

// TopN is how many largest items per library, and newest items per
// server, the exporter publishes.
const TopN = 10

// Last-play buckets of the watch-age families.
const (
	WatchedNever    = "never"
	WatchedOver1y   = "over_1y"
	Watched90dTo1y  = "90d_1y"
	WatchedUnder90d = "under_90d"
)

// WatchBuckets lists the buckets in display order.
var WatchBuckets = []string{WatchedNever, WatchedOver1y, Watched90dTo1y, WatchedUnder90d}

const daySeconds = 24 * 60 * 60

// ErrByteOverflow reports a library whose byte total does not fit int64,
// so no figure from that walk can be published as a real size.
var ErrByteOverflow = errors.New("library byte total exceeds int64")

// Item is one ranked entry: a movie or other video, or a whole show. Episode
// is "SxxEyy" for a show's newest episode in a recently-added list, else "".
// ShowKey is the show's rating key on a show entry, else 0; an episode row
// carries no show year, so Aggregate leaves Year 0 there. Aggregate leaves
// the library fields empty for the caller to fill.
type Item struct {
	LibraryID   string
	LibraryType string
	Title       string
	Episode     string
	RatingKey   uint64
	ShowKey     uint64
	Bytes       int64
	LastPlayed  int64
	AddedAt     int64
	Year        int
}

// Stats is one library's walk result. Byte figures count sized items only.
type Stats struct {
	ResolutionBytes   map[string]int64
	CodecBytes        map[string]int64
	WatchAgeBytes     map[string]int64
	WatchAgeItems     map[string]int64
	Largest           []Item
	Recent            []Item
	MultiVersionItems int64
	ExtraVersionBytes int64
	NewestAdded       int64
	Added24h          int64
	Added7d           int64
	Unsized           int64
}

// LastPlayed returns the newest recorded play of a rating key by any
// account, as a Unix time, or 0 when none is recorded.
type LastPlayed func(ratingKey uint64) int64

// Aggregate reads every item a walk yields and returns the library's
// figures, or the walk's first error, or ErrByteOverflow. In a show library
// each item is an episode, and the largest and newest lists rank whole
// shows. Offset paging can return a row twice while the library changes, so
// a rating key counts only the first time it is read; a row the shift skips
// is missed until the next walk. played is called once
// for every distinct item key and show key.
func Aggregate(items iter.Seq2[plexapi.Item, error], showLibrary bool, played LastPlayed, now time.Time) (Stats, error) {
	a := newAccumulator(played, now)
	for it, err := range items {
		if err != nil {
			return Stats{}, err
		}
		a.add(&it, showLibrary)
		if a.overflow {
			return Stats{}, ErrByteOverflow
		}
	}
	return a.finish(), nil
}

type show struct {
	title      string
	newest     Item
	key        uint64
	bytes      int64
	lastPlayed int64
	// partial is set once any episode is unsized or the sum overflows; such
	// a show's total is unknown, so it is not ranked.
	partial bool
}

type accumulator struct {
	played   LastPlayed
	shows    map[uint64]*show
	seen     keySet
	stats    Stats
	now      int64
	overflow bool
}

func newAccumulator(played LastPlayed, now time.Time) *accumulator {
	return &accumulator{
		played: played,
		now:    now.Unix(),
		shows:  make(map[uint64]*show),
		seen:   newKeySet(),
		stats: Stats{
			ResolutionBytes: make(map[string]int64),
			CodecBytes:      make(map[string]int64),
			WatchAgeBytes:   make(map[string]int64),
			WatchAgeItems:   make(map[string]int64),
		},
	}
}

func (a *accumulator) add(it *plexapi.Item, showLibrary bool) {
	key, err := strconv.ParseUint(it.RatingKey, 10, 64)
	if err != nil {
		return
	}
	if !a.seen.insert(key) {
		return
	}
	bytes, media, sized := itemBytes(it)
	last := a.lastPlayed(key, it)
	a.countAdded(it.AddedAt)

	a.stats.WatchAgeItems[a.bucket(last)]++
	if sized {
		a.addTo(a.stats.WatchAgeBytes, a.bucket(last), bytes)
		a.countMedia(it.Media, media, bytes)
	} else {
		a.stats.Unsized++
	}

	if !showLibrary {
		entry := Item{RatingKey: key, Title: it.Title, Year: it.Year, Bytes: bytes, LastPlayed: last, AddedAt: it.AddedAt}
		if sized {
			a.stats.Largest = insertTop(a.stats.Largest, &entry, bySize)
		}
		if it.AddedAt > 0 {
			a.stats.Recent = insertTop(a.stats.Recent, &entry, byAdded)
		}
		return
	}
	a.addEpisode(it, key, bytes, sized, last)
}

func (a *accumulator) addEpisode(it *plexapi.Item, key uint64, bytes int64, sized bool, last int64) {
	showKey, err := strconv.ParseUint(it.GrandparentRatingKey, 10, 64)
	if err != nil {
		return
	}
	s := a.shows[showKey]
	if s == nil {
		s = &show{key: showKey, title: it.GrandparentTitle, lastPlayed: a.played(showKey)}
		a.shows[showKey] = s
	}
	if !sized || bytes > math.MaxInt64-s.bytes {
		s.partial = true
	} else {
		s.bytes += bytes
	}
	s.lastPlayed = max(s.lastPlayed, last)
	if it.AddedAt > 0 && byAdded(&Item{RatingKey: key, AddedAt: it.AddedAt}, &s.newest) < 0 {
		s.newest = Item{
			RatingKey: key, ShowKey: showKey, Title: it.GrandparentTitle, AddedAt: it.AddedAt,
			Episode: fmt.Sprintf("S%02dE%02d", it.SeasonNum(), it.EpisodeNum()),
		}
	}
}

// lastPlayed is the newer of the history play and the token owner's own
// watch state; the owner state counts only with a positive view count.
func (a *accumulator) lastPlayed(key uint64, it *plexapi.Item) int64 {
	last := a.played(key)
	if it.ViewCount != nil && *it.ViewCount > 0 && it.LastViewedAt != nil {
		last = max(last, int64(*it.LastViewedAt))
	}
	return max(last, 0)
}

func (a *accumulator) bucket(last int64) string {
	// Ages stay in seconds: a far-off time converted to a Duration overflows.
	age := a.now - last
	switch {
	case last <= 0:
		return WatchedNever
	case age < 90*daySeconds:
		return WatchedUnder90d
	case age < 365*daySeconds:
		return Watched90dTo1y
	default:
		return WatchedOver1y
	}
}

// countAdded counts an item in the added windows. An addedAt <= 0 is
// unknown: it is older than any window and never the newest.
func (a *accumulator) countAdded(addedAt int64) {
	if addedAt <= 0 {
		return
	}
	a.stats.NewestAdded = max(a.stats.NewestAdded, addedAt)
	age := a.now - addedAt
	if age <= daySeconds {
		a.stats.Added24h++
	}
	if age <= 7*daySeconds {
		a.stats.Added7d++
	}
}

// countMedia adds a sized item's per-version bytes; every version but the
// largest counts as extra.
func (a *accumulator) countMedia(media []plexapi.Media, sizes []int64, total int64) {
	var largest int64
	for i := range media {
		a.addTo(a.stats.ResolutionBytes, metrics.ResolutionBucket(media[i].VideoResolution), sizes[i])
		a.addTo(a.stats.CodecBytes, metrics.NormalizeCodec(media[i].VideoCodec), sizes[i])
		largest = max(largest, sizes[i])
	}
	if len(media) > 1 {
		a.stats.MultiVersionItems++
		a.stats.ExtraVersionBytes = a.checkedAdd(a.stats.ExtraVersionBytes, total-largest)
	}
}

func (a *accumulator) addTo(m map[string]int64, key string, n int64) {
	m[key] = a.checkedAdd(m[key], n)
}

// checkedAdd adds two non-negative byte counts and marks the walk failed
// when the sum does not fit.
func (a *accumulator) checkedAdd(sum, n int64) int64 {
	if n > math.MaxInt64-sum {
		a.overflow = true
		return sum
	}
	return sum + n
}

func (a *accumulator) finish() Stats {
	for _, s := range a.shows {
		if !s.partial {
			a.stats.Largest = insertTop(a.stats.Largest, &Item{RatingKey: s.key, ShowKey: s.key, Title: s.title, Bytes: s.bytes, LastPlayed: s.lastPlayed}, bySize)
		}
		if s.newest.AddedAt > 0 {
			a.stats.Recent = insertTop(a.stats.Recent, &s.newest, byAdded)
		}
	}
	return a.stats
}

// itemBytes sums every part of every version. An item is unsized when it
// has no version, a version has no part, a part has no positive size, or
// the sum overflows: one unknown version makes the whole total unknown.
func itemBytes(it *plexapi.Item) (total int64, perMedia []int64, sized bool) {
	if len(it.Media) == 0 {
		return 0, nil, false
	}
	perMedia = make([]int64, len(it.Media))
	for i := range it.Media {
		if len(it.Media[i].Part) == 0 {
			return 0, nil, false
		}
		for _, p := range it.Media[i].Part {
			if p.Size == nil || *p.Size <= 0 || int64(*p.Size) > math.MaxInt64-total {
				return 0, nil, false
			}
			perMedia[i] += int64(*p.Size)
			total += int64(*p.Size)
		}
	}
	return total, perMedia, true
}

func bySize(a, b *Item) int {
	return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.RatingKey, b.RatingKey))
}

func byAdded(a, b *Item) int {
	return cmp.Or(cmp.Compare(b.AddedAt, a.AddedAt), cmp.Compare(a.RatingKey, b.RatingKey))
}

// insertTop keeps list sorted by order and at most TopN long.
func insertTop(list []Item, it *Item, order func(a, b *Item) int) []Item {
	i := len(list)
	for i > 0 && order(it, &list[i-1]) < 0 {
		i--
	}
	if i == TopN {
		return list
	}
	list = slices.Insert(list, i, *it)
	if len(list) > TopN {
		list = list[:TopN]
	}
	return list
}

// MergeRecent returns the TopN newest items across several libraries' lists.
func MergeRecent(lists ...[]Item) []Item {
	var out []Item
	for _, l := range lists {
		for i := range l {
			out = insertTop(out, &l[i], byAdded)
		}
	}
	return out
}
