package libstats

import "slices"

// keySet holds a walk's rating keys; a map of 500,000 keys raised peak RSS
// by 25 MiB. Plex numbers items densely, so keys group by their high 48 bits
// and a group keeps a sorted list of low halves until a bitmap of the whole
// group is smaller. A dense library costs about two bytes a key, plus list
// capacity slack and one map entry per group.
type keySet struct {
	groups map[uint64]*keyGroup
}

type keyGroup struct {
	bits *[1 << 10]uint64
	low  []uint16
}

// groupListMax is the list length at which the 8 KiB bitmap is smaller.
const groupListMax = 4096

func newKeySet() keySet {
	return keySet{groups: make(map[uint64]*keyGroup)}
}

// insert adds key and reports whether it was absent.
func (s keySet) insert(key uint64) bool {
	g := s.groups[key>>16]
	if g == nil {
		g = &keyGroup{}
		s.groups[key>>16] = g
	}
	return g.insert(uint16(key & 0xffff))
}

func (g *keyGroup) insert(v uint16) bool {
	if g.bits != nil {
		word, bit := v>>6, uint64(1)<<(v&63)
		if g.bits[word]&bit != 0 {
			return false
		}
		g.bits[word] |= bit
		return true
	}
	i, found := slices.BinarySearch(g.low, v)
	if found {
		return false
	}
	if len(g.low) < groupListMax {
		g.low = slices.Insert(g.low, i, v)
		return true
	}
	g.bits = new([1 << 10]uint64)
	for _, x := range g.low {
		g.bits[x>>6] |= 1 << (x & 63)
	}
	g.low = nil
	return g.insert(v)
}
