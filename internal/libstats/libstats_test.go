package libstats_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/libstats"
	"github.com/cplieger/plexapi/v2"
	"pgregory.net/rapid"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) int64 { return now.Add(-d).Unix() }

// items decodes a JSON array of Plex section rows, the wire shape a
// library page carries, so the fixtures exercise plexapi's presence rules.
func items(t testing.TB, rows string) iter.Seq2[plexapi.Item, error] {
	t.Helper()
	var list []plexapi.Item
	if err := json.Unmarshal([]byte(rows), &list); err != nil {
		t.Fatalf("Setup: decode rows: %v", err)
	}
	return func(yield func(plexapi.Item, error) bool) {
		for _, it := range list {
			if !yield(it, nil) {
				return
			}
		}
	}
}

func noPlays(uint64) int64 { return 0 }

func movie(key, size int, extra string) string {
	return fmt.Sprintf(`{"ratingKey":"%d","title":"M%d","year":2020,"addedAt":%d%s,
		"Media":[{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":%d}]}]}`,
		key, key, ago(30*24*time.Hour), extra, size)
}

func TestAggregate_unsized_part_leaves_item_out_of_byte_figures(t *testing.T) {
	rows := `[` + movie(1, 5000, "") + `,
		{"ratingKey":"2","title":"No size","addedAt":0,"Media":[{"videoResolution":"4k","Part":[{"id":3}]}]},
		{"ratingKey":"3","title":"Null size","Media":[{"Part":[{"size":null}]}]},
		{"ratingKey":"4","title":"Zero size","Media":[{"Part":[{"size":0}]}]},
		{"ratingKey":"5","title":"No parts","Media":[]}]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.Unsized != 4 {
		t.Errorf("Unsized = %d, want 4", st.Unsized)
	}
	if len(st.Largest) != 1 || st.Largest[0].RatingKey != 1 {
		t.Errorf("Largest = %+v, want only item 1", st.Largest)
	}
	if got := st.ResolutionBytes["2160"]; got != 0 {
		t.Errorf("ResolutionBytes[2160] = %d, want 0: the 4k item has no size", got)
	}
	if got := st.WatchAgeBytes[libstats.WatchedNever]; got != 5000 {
		t.Errorf("WatchAgeBytes[never] = %d, want 5000", got)
	}
	if got := st.WatchAgeItems[libstats.WatchedNever]; got != 5 {
		t.Errorf("WatchAgeItems[never] = %d, want 5: an item counts whatever its size", got)
	}
}

func TestAggregate_unknown_added_time_is_in_no_window(t *testing.T) {
	rows := `[{"ratingKey":"1","title":"A","addedAt":0,"Media":[{"Part":[{"size":1}]}]},
		{"ratingKey":"2","title":"B","addedAt":-5,"Media":[{"Part":[{"size":1}]}]}]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.Added24h != 0 || st.Added7d != 0 || st.NewestAdded != 0 || len(st.Recent) != 0 {
		t.Errorf("Added24h=%d Added7d=%d NewestAdded=%d Recent=%v, want all empty",
			st.Added24h, st.Added7d, st.NewestAdded, st.Recent)
	}
}

// A far-negative added time is still unknown: converting its age to a
// time.Duration would overflow into a negative age inside both windows.
func TestAggregate_far_negative_added_time_is_in_no_window(t *testing.T) {
	rows := `[{"ratingKey":"1","title":"A","addedAt":-10000000000,"Media":[{"Part":[{"size":1}]}]},
		{"ratingKey":"2","title":"B","addedAt":-9223372036854775808,"Media":[{"Part":[{"size":1}]}]}]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.Added24h != 0 || st.Added7d != 0 || st.NewestAdded != 0 || len(st.Recent) != 0 {
		t.Errorf("Added24h=%d Added7d=%d NewestAdded=%d Recent=%v, want all empty",
			st.Added24h, st.Added7d, st.NewestAdded, st.Recent)
	}
}

// A last play far in the future is recent, not wrapped into an older
// bucket by a Duration overflow.
func TestAggregate_far_future_last_play_is_recent(t *testing.T) {
	// 18,415,000,000 s ahead of now wraps to a 367-day age in nanoseconds.
	rows := movie(1, 100, fmt.Sprintf(`,"viewCount":1,"lastViewedAt":%d`, now.Unix()+18_415_000_000))
	st, err := libstats.Aggregate(items(t, "["+rows+"]"), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if got := st.WatchAgeItems[libstats.WatchedUnder90d]; got != 1 {
		t.Errorf("WatchAgeItems = %v, want the item under 90 days", st.WatchAgeItems)
	}
}

func TestAggregate_added_windows(t *testing.T) {
	rows := fmt.Sprintf(`[{"ratingKey":"1","addedAt":%d},{"ratingKey":"2","addedAt":%d},{"ratingKey":"3","addedAt":%d}]`,
		ago(time.Hour), ago(3*24*time.Hour), ago(8*24*time.Hour))
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.Added24h != 1 || st.Added7d != 2 || st.NewestAdded != ago(time.Hour) {
		t.Errorf("Added24h=%d Added7d=%d NewestAdded=%d, want 1, 2, %d", st.Added24h, st.Added7d, st.NewestAdded, ago(time.Hour))
	}
}

func TestAggregate_watch_age_buckets_from_history_and_owner_state(t *testing.T) {
	d := 24 * time.Hour
	played := map[uint64]int64{1: ago(10 * d), 2: ago(90 * d), 3: ago(365 * d), 6: ago(400 * d)}
	rows := `[` + strings.Join([]string{
		movie(1, 1, ""), movie(2, 2, ""), movie(3, 4, ""), movie(4, 8, ""),
		movie(5, 16, fmt.Sprintf(`,"viewCount":2,"lastViewedAt":%d`, ago(5*d))),
		movie(6, 32, fmt.Sprintf(`,"viewCount":0,"lastViewedAt":%d`, ago(5*d))),
	}, ",") + `]`
	st, err := libstats.Aggregate(items(t, rows), false, func(k uint64) int64 { return played[k] }, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	want := map[string]int64{
		libstats.WatchedUnder90d: 1 + 16, libstats.Watched90dTo1y: 2,
		libstats.WatchedOver1y: 4 + 32, libstats.WatchedNever: 8,
	}
	for b, n := range want {
		if st.WatchAgeBytes[b] != n {
			t.Errorf("WatchAgeBytes[%s] = %d, want %d", b, st.WatchAgeBytes[b], n)
		}
	}
}

func TestAggregate_largest_ranks_shows_by_summed_episode_bytes(t *testing.T) {
	ep := func(key, show, season, idx, size int, added int64) string {
		return fmt.Sprintf(`{"ratingKey":"%d","grandparentRatingKey":"%d","grandparentTitle":"Show %d",
			"parentIndex":%d,"index":%d,"addedAt":%d,"Media":[{"Part":[{"size":%d}]}]}`,
			key, show, show, season, idx, added, size)
	}
	rows := `[` + strings.Join([]string{
		ep(11, 100, 1, 1, 60, ago(5*time.Hour)), ep(12, 100, 1, 2, 60, ago(2*time.Hour)),
		ep(21, 200, 3, 7, 100, ago(time.Hour)),
	}, ",") + `]`
	played := map[uint64]int64{11: ago(time.Hour)}
	st, err := libstats.Aggregate(items(t, rows), true, func(k uint64) int64 { return played[k] }, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if len(st.Largest) != 2 || st.Largest[0].RatingKey != 100 || st.Largest[0].ShowKey != 100 || st.Largest[0].Bytes != 120 {
		t.Fatalf("Largest = %+v, want show 100 (120 bytes, show key 100) first", st.Largest)
	}
	if st.Largest[0].LastPlayed != ago(time.Hour) || st.Largest[1].LastPlayed != 0 {
		t.Errorf("Largest last played = %d, %d, want %d, 0", st.Largest[0].LastPlayed, st.Largest[1].LastPlayed, ago(time.Hour))
	}
	want := []libstats.Item{
		{RatingKey: 21, ShowKey: 200, Title: "Show 200", Episode: "S03E07", AddedAt: ago(time.Hour)},
		{RatingKey: 12, ShowKey: 100, Title: "Show 100", Episode: "S01E02", AddedAt: ago(2 * time.Hour)},
	}
	if !slices.Equal(st.Recent, want) {
		t.Errorf("Recent = %+v, want %+v (each show once, with its newest episode)", st.Recent, want)
	}
}

func TestAggregate_show_with_an_unsized_episode_is_not_ranked(t *testing.T) {
	ep := func(key, show int, media string) string {
		return fmt.Sprintf(`{"ratingKey":"%d","grandparentRatingKey":"%d","grandparentTitle":"Show %d",
			"parentIndex":1,"index":%d,"Media":%s}`, key, show, show, key, media)
	}
	rows := `[` + strings.Join([]string{
		ep(11, 100, `[{"Part":[{"size":100000}]}]`), ep(12, 100, `[{"Part":[{"id":5}]}]`),
		ep(21, 200, `[{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":9223372036854775000}]}]`),
		ep(22, 200, `[{"videoResolution":"720","videoCodec":"hevc","Part":[{"size":9000}]}]`),
		ep(31, 300, `[{"Part":[{"size":10}]}]`),
	}, ",") + `]`
	// Episode 21 sits in its own watch bucket, resolution and codec, so only
	// the show's own total overflows.
	played := func(k uint64) int64 {
		if k == 21 {
			return ago(time.Hour)
		}
		return 0
	}
	st, err := libstats.Aggregate(items(t, rows), true, played, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if len(st.Largest) != 1 || st.Largest[0].RatingKey != 300 || st.Largest[0].Bytes != 10 {
		t.Errorf("Largest = %+v, want only show 300: show 100 has an unsized episode and show 200 overflows", st.Largest)
	}
}

func TestAggregate_largest_ties_break_by_rating_key(t *testing.T) {
	rows := `[` + movie(30, 7, "") + `,` + movie(4, 7, "") + `,` + movie(200, 7, "") + `]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	var keys []uint64
	for _, it := range st.Largest {
		keys = append(keys, it.RatingKey)
	}
	if !slices.Equal(keys, []uint64{4, 30, 200}) {
		t.Errorf("Largest keys = %v, want [4 30 200] (numeric order on equal size)", keys)
	}
}

func TestAggregate_extra_versions(t *testing.T) {
	rows := `[{"ratingKey":"1","Media":[
		{"videoResolution":"2160","videoCodec":"hevc","Part":[{"size":700}]},
		{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":200},{"size":100}]}]}]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.MultiVersionItems != 1 || st.ExtraVersionBytes != 300 {
		t.Errorf("MultiVersionItems=%d ExtraVersionBytes=%d, want 1, 300", st.MultiVersionItems, st.ExtraVersionBytes)
	}
	if st.ResolutionBytes["2160"] != 700 || st.CodecBytes["h264"] != 300 || st.Largest[0].Bytes != 1000 {
		t.Errorf("ResolutionBytes=%v CodecBytes=%v Largest=%+v", st.ResolutionBytes, st.CodecBytes, st.Largest)
	}
}

func TestAggregate_version_without_parts_leaves_the_whole_item_unsized(t *testing.T) {
	rows := `[` + movie(1, 50, "") + `,
		{"ratingKey":"2","title":"Partial","Media":[
			{"videoResolution":"2160","videoCodec":"hevc"},
			{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":100}]}]},
		{"ratingKey":"3","title":"Empty parts","Media":[
			{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":200}]},
			{"videoResolution":"720","videoCodec":"h264","Part":[]}]}]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.Unsized != 2 {
		t.Errorf("Unsized = %d, want 2", st.Unsized)
	}
	if len(st.Largest) != 1 || st.Largest[0].RatingKey != 1 {
		t.Errorf("Largest = %+v, want only item 1", st.Largest)
	}
	if st.MultiVersionItems != 0 || st.ExtraVersionBytes != 0 {
		t.Errorf("MultiVersionItems=%d ExtraVersionBytes=%d, want 0, 0", st.MultiVersionItems, st.ExtraVersionBytes)
	}
	if st.ResolutionBytes["1080"] != 50 || st.CodecBytes["h264"] != 50 || st.WatchAgeBytes[libstats.WatchedNever] != 50 {
		t.Errorf("ResolutionBytes=%v CodecBytes=%v WatchAgeBytes=%v, want only item 1's 50 bytes",
			st.ResolutionBytes, st.CodecBytes, st.WatchAgeBytes)
	}
}

func TestAggregate_show_with_a_partless_episode_version_is_not_ranked(t *testing.T) {
	rows := `[{"ratingKey":"11","grandparentRatingKey":"100","grandparentTitle":"Show 100","parentIndex":1,"index":1,
			"Media":[{},{"Part":[{"size":100}]}]},
		{"ratingKey":"21","grandparentRatingKey":"200","grandparentTitle":"Show 200","parentIndex":1,"index":1,
			"Media":[{"Part":[{"size":10}]}]}]`
	st, err := libstats.Aggregate(items(t, rows), true, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if len(st.Largest) != 1 || st.Largest[0].RatingKey != 200 {
		t.Errorf("Largest = %+v, want only show 200", st.Largest)
	}
}

// Offset paging can return a row twice while the library changes; each
// rating key counts once in every figure and ranked list.
func TestAggregate_repeated_movie_row_counts_once(t *testing.T) {
	multi := fmt.Sprintf(`{"ratingKey":"2","title":"Two cuts","addedAt":%d,"Media":[
		{"videoResolution":"2160","videoCodec":"hevc","Part":[{"size":700}]},
		{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":300}]}]}`, ago(time.Hour))
	unsized := `{"ratingKey":"3","title":"No size","Media":[{"Part":[{"id":1}]}]}`
	rows := `[` + strings.Join([]string{movie(1, 50, ""), multi, unsized, movie(1, 50, ""), multi, unsized}, ",") + `]`
	st, err := libstats.Aggregate(items(t, rows), false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if got := st.WatchAgeItems[libstats.WatchedNever]; got != 3 {
		t.Errorf("WatchAgeItems[never] = %d, want 3", got)
	}
	if got := st.WatchAgeBytes[libstats.WatchedNever]; got != 1050 {
		t.Errorf("WatchAgeBytes[never] = %d, want 1050", got)
	}
	if st.ResolutionBytes["2160"] != 700 || st.ResolutionBytes["1080"] != 350 {
		t.Errorf("ResolutionBytes = %v, want 2160:700 1080:350", st.ResolutionBytes)
	}
	if st.CodecBytes["hevc"] != 700 || st.CodecBytes["h264"] != 350 {
		t.Errorf("CodecBytes = %v, want hevc:700 h264:350", st.CodecBytes)
	}
	if st.MultiVersionItems != 1 || st.ExtraVersionBytes != 300 {
		t.Errorf("MultiVersionItems=%d ExtraVersionBytes=%d, want 1, 300", st.MultiVersionItems, st.ExtraVersionBytes)
	}
	if st.Added24h != 1 || st.Added7d != 1 || st.Unsized != 1 {
		t.Errorf("Added24h=%d Added7d=%d Unsized=%d, want 1, 1, 1", st.Added24h, st.Added7d, st.Unsized)
	}
	if keys := rankedKeys(st.Largest); !slices.Equal(keys, []uint64{2, 1}) {
		t.Errorf("Largest keys = %v, want [2 1]", keys)
	}
	if keys := rankedKeys(st.Recent); !slices.Equal(keys, []uint64{2, 1}) {
		t.Errorf("Recent keys = %v, want [2 1]", keys)
	}
}

func TestAggregate_repeated_episode_row_counts_once(t *testing.T) {
	ep := func(key, size int) string {
		return fmt.Sprintf(`{"ratingKey":"%d","grandparentRatingKey":"100","grandparentTitle":"Show","parentIndex":1,"index":%d,
			"addedAt":%d,"Media":[{"videoResolution":"1080","videoCodec":"h264","Part":[{"size":%d}]}]}`, key, key, ago(time.Hour), size)
	}
	rows := `[` + strings.Join([]string{ep(11, 60), ep(12, 40), ep(11, 60)}, ",") + `]`
	st, err := libstats.Aggregate(items(t, rows), true, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if st.WatchAgeItems[libstats.WatchedNever] != 2 || st.WatchAgeBytes[libstats.WatchedNever] != 100 {
		t.Errorf("WatchAgeItems=%v WatchAgeBytes=%v, want 2 items and 100 bytes", st.WatchAgeItems, st.WatchAgeBytes)
	}
	if st.ResolutionBytes["1080"] != 100 || st.CodecBytes["h264"] != 100 || st.Added24h != 2 {
		t.Errorf("ResolutionBytes=%v CodecBytes=%v Added24h=%d, want 100, 100, 2", st.ResolutionBytes, st.CodecBytes, st.Added24h)
	}
	if len(st.Largest) != 1 || st.Largest[0].RatingKey != 100 || st.Largest[0].Bytes != 100 {
		t.Errorf("Largest = %+v, want show 100 at 100 bytes", st.Largest)
	}
	if keys := rankedKeys(st.Recent); !slices.Equal(keys, []uint64{11}) {
		t.Errorf("Recent keys = %v, want [11]", keys)
	}
}

func rankedKeys(list []libstats.Item) []uint64 {
	keys := make([]uint64, 0, len(list))
	for _, it := range list {
		keys = append(keys, it.RatingKey)
	}
	return keys
}

// Each item's size fits int64, but two or more together do not. Every case
// overflows exactly one library total and keeps the others in range.
func TestAggregate_library_byte_total_overflow_fails_the_walk(t *testing.T) {
	const half = math.MaxInt64/2 + 1
	const quarter = math.MaxInt64/4 + 1
	d := 24 * time.Hour
	played := map[uint64]int64{2: ago(10 * d), 3: ago(200 * d), 4: ago(400 * d)}
	media := func(res, codec string, size int64) string {
		return fmt.Sprintf(`{"videoResolution":%q,"videoCodec":%q,"Part":[{"size":%d}]}`, res, codec, size)
	}
	row := func(key int, m ...string) string {
		return fmt.Sprintf(`{"ratingKey":"%d","Media":[%s]}`, key, strings.Join(m, ","))
	}
	cases := []struct {
		name string
		rows []string
	}{
		{"watch_age", []string{row(1, media("1080", "h264", half)), row(5, media("720", "hevc", half))}},
		{"resolution", []string{row(1, media("1080", "h264", half)), row(2, media("1080", "hevc", half))}},
		{"codec", []string{row(1, media("1080", "h264", half)), row(2, media("720", "h264", half))}},
		{"extra_version", []string{
			row(1, media("2160", "h264", quarter), media("1080", "hevc", quarter)),
			row(2, media("720", "av1", quarter), media("576", "vp9", quarter)),
			row(3, media("480", "mpeg2video", quarter), media("sd", "mpeg4", quarter)),
			row(4, media("other", "vc1", quarter), media("", "other", quarter)),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := libstats.Aggregate(items(t, "["+strings.Join(tc.rows, ",")+"]"), false,
				func(k uint64) int64 { return played[k] }, now)
			if !errors.Is(err, libstats.ErrByteOverflow) {
				t.Errorf("Aggregate(%s) error = %v, want %v; stats %+v", tc.name, err, libstats.ErrByteOverflow, st)
			}
		})
	}
}

func TestAggregate_returns_the_walk_error(t *testing.T) {
	boom := errors.New("page 3 failed")
	seq := func(yield func(plexapi.Item, error) bool) {
		if yield(plexapi.Item{RatingKey: "1"}, nil) {
			yield(plexapi.Item{}, boom)
		}
	}
	if _, err := libstats.Aggregate(seq, false, noPlays, now); !errors.Is(err, boom) {
		t.Errorf("Aggregate error = %v, want %v", err, boom)
	}
}

// The ranking must equal a full sort of the same items, however many there
// are and whatever order they arrive in.
func TestAggregate_largest_matches_full_sort(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 60).Draw(t, "n")
		sizes := rapid.SliceOfN(rapid.IntRange(1, 20), n, n).Draw(t, "sizes")
		var rows []string
		for i, s := range sizes {
			rows = append(rows, movie(i+1, s, ""))
		}
		var list []plexapi.Item
		if err := json.Unmarshal([]byte("["+strings.Join(rows, ",")+"]"), &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		st, err := libstats.Aggregate(func(yield func(plexapi.Item, error) bool) {
			for _, it := range list {
				if !yield(it, nil) {
					return
				}
			}
		}, false, noPlays, now)
		if err != nil {
			t.Fatalf("Aggregate error = %v", err)
		}
		type kv struct{ size, key int }
		var want []kv
		for i, s := range sizes {
			want = append(want, kv{s, i + 1})
		}
		slices.SortFunc(want, func(a, b kv) int {
			if a.size != b.size {
				return b.size - a.size
			}
			return a.key - b.key
		})
		want = want[:min(len(want), libstats.TopN)]
		if len(st.Largest) != len(want) {
			t.Fatalf("len(Largest) = %d, want %d", len(st.Largest), len(want))
		}
		for i, w := range want {
			if got := st.Largest[i]; got.RatingKey != uint64(w.key) || got.Bytes != int64(w.size) {
				t.Fatalf("Largest[%d] = key %d size %d, want key %d size %d", i, got.RatingKey, got.Bytes, w.key, w.size)
			}
		}
	})
}

func TestAggregate_bounded_for_a_large_library(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	seq := func(yield func(plexapi.Item, error) bool) {
		for i := range 10_000 {
			size := plexapi.FlexInt64(r.Int64N(1 << 40))
			it := plexapi.Item{
				RatingKey: fmt.Sprint(i + 1), AddedAt: ago(time.Duration(r.IntN(1000)) * time.Hour),
				Media: []plexapi.Media{{Part: []plexapi.Part{{Size: &size}}}},
			}
			if !yield(it, nil) {
				return
			}
		}
	}
	st, err := libstats.Aggregate(seq, false, noPlays, now)
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	if len(st.Largest) != libstats.TopN || len(st.Recent) != libstats.TopN {
		t.Errorf("len(Largest)=%d len(Recent)=%d, want %d each", len(st.Largest), len(st.Recent), libstats.TopN)
	}
	if n := len(st.ResolutionBytes) + len(st.CodecBytes) + len(st.WatchAgeBytes); n > 8+9+4 {
		t.Errorf("%d aggregate label values, want at most 21", n)
	}
}

func TestMergeRecent_keeps_the_newest_across_libraries(t *testing.T) {
	a := []libstats.Item{{RatingKey: 1, AddedAt: 50}, {RatingKey: 2, AddedAt: 10}}
	b := []libstats.Item{{RatingKey: 3, AddedAt: 40}}
	got := libstats.MergeRecent(a, b)
	if len(got) != 3 || got[0].RatingKey != 1 || got[1].RatingKey != 3 || got[2].RatingKey != 2 {
		t.Errorf("MergeRecent = %+v, want keys 1, 3, 2", got)
	}
}
