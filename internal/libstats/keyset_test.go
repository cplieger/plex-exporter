package libstats

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

// insert must agree with a map on every key, across the switch from a
// group's list to its bitmap and at the extremes of the key range.
func TestKeySet_insert_matches_a_map(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		key := rapid.OneOf(
			rapid.Uint64Range(0, 70_000),
			rapid.Uint64Range(math.MaxUint64-70_000, math.MaxUint64),
			rapid.Custom(func(t *rapid.T) uint64 {
				return rapid.Uint64Range(0, 3).Draw(t, "high")<<rapid.SampledFrom([]int{16, 32, 48}).Draw(t, "shift") |
					rapid.Uint64Range(0, 3).Draw(t, "low")
			}),
		)
		keys := rapid.SliceOfN(key, 0, 9000).Draw(t, "keys")
		s, want := newKeySet(), make(map[uint64]bool)
		for i, k := range keys {
			if got := s.insert(k); got != !want[k] {
				t.Fatalf("insert(%d) at step %d = %v, want %v", k, i, got, !want[k])
			}
			want[k] = true
		}
	})
}

// A group that outgrows its list keeps every key it held when it switches
// to a bitmap.
func TestKeySet_keeps_keys_across_the_bitmap_switch(t *testing.T) {
	s := newKeySet()
	const base = 7 << 16
	for k := range uint64(groupListMax + 1) {
		if !s.insert(base + 2*k) {
			t.Fatalf("first insert(%d) = false, want true", base+2*k)
		}
	}
	for k := range uint64(groupListMax + 1) {
		if s.insert(base + 2*k) {
			t.Fatalf("repeat insert(%d) = true, want false", base+2*k)
		}
		if !s.insert(base + 2*k + 1) {
			t.Fatalf("first insert(%d) = false, want true", base+2*k+1)
		}
	}
}
