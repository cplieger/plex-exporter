package server

import (
	"bufio"
	"context"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-exporter/internal/history"
	"github.com/cplieger/plex-exporter/internal/libstats"
	"github.com/cplieger/plexapi/v2"
)

const (
	memoryChildEnv   = "PLEX_EXPORTER_MEMORY_CHILD"
	memoryBudget     = 48 << 20
	memHistoryRows   = 250_000
	memShowLibLeaves = 500_000
)

type syntheticHistory struct{}

func (syntheticHistory) WalkHistory(_ context.Context, _ int64, _ plexapi.Page, _ func(context.Context) error) iter.Seq2[plexapi.HistoryEntry, error] {
	return func(yield func(plexapi.HistoryEntry, error) bool) {
		for i := range memHistoryRows {
			e := plexapi.HistoryEntry{RatingKey: strconv.Itoa(1 + i%memShowLibLeaves), ViewedAt: int64(1_600_000_000 + i), HistoryKey: "/status/sessions/history/" + strconv.Itoa(i)}
			if !yield(e, nil) {
				return
			}
		}
	}
}

func syntheticShowLibrary(yield func(plexapi.Item, error) bool) {
	for i := range memShowLibLeaves {
		size := plexapi.FlexInt64(1_000_000 + i)
		it := plexapi.Item{
			RatingKey: strconv.Itoa(i + 1), GrandparentRatingKey: strconv.Itoa(1_000_000 + i/50),
			GrandparentTitle: "Show " + strconv.Itoa(i/50), AddedAt: int64(1_600_000_000 + i),
			Media: []plexapi.Media{{VideoResolution: "1080", VideoCodec: "hevc", Part: []plexapi.Part{{Size: &size}}}},
		}
		if !yield(it, nil) {
			return
		}
	}
}

// The child builds the watched map from 250,000 history rows and walks a
// 500,000-episode library, then reports its peak resident size and the
// heap and stacks it keeps.
func TestMemoryChild(t *testing.T) {
	if os.Getenv(memoryChildEnv) == "" {
		t.Skip("runs only as the child of TestMemory_large_library_and_history_stay_bounded")
	}
	h := history.New(syntheticHistory{}, noPace{})
	if st := h.Bootstrap(t.Context()); st != history.StatusComplete {
		t.Fatalf("Bootstrap() = %q", st)
	}
	keys := 0
	stats, err := libstats.Aggregate(syntheticShowLibrary, true, func(k uint64) int64 { keys++; return h.Visit(k, 1) }, time.Now())
	if err != nil {
		t.Fatalf("Aggregate error = %v", err)
	}
	h.Prune(1, 0, h.View().Gen)
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("MEMORY retained=%d hwm=%d keys=%d largest=%d\n", ms.HeapInuse+ms.StackInuse, vmHWM(t), keys, len(stats.Largest))
	runtime.KeepAlive(h)
}

func vmHWM(t *testing.T) uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmHWM:"); ok {
			kb, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
			return kb << 10
		}
	}
	return 0
}

func TestMemory_large_library_and_history_stay_bounded(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a 500,000-item library")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMemoryChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), memoryChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	var retained, hwm uint64
	var keys, largest int
	line := out[strings.Index(string(out), "MEMORY"):]
	if _, err := fmt.Sscanf(string(line), "MEMORY retained=%d hwm=%d keys=%d largest=%d", &retained, &hwm, &keys, &largest); err != nil {
		t.Fatalf("parse child output %q: %v", out, err)
	}
	t.Logf("retained heap and stacks %d MiB, peak RSS %d MiB", retained>>20, hwm>>20)
	if retained > memoryBudget {
		t.Errorf("retained heap and stacks = %d MiB, want at most %d MiB", retained>>20, memoryBudget>>20)
	}
	if !raceEnabled && hwm > memoryBudget {
		t.Errorf("peak RSS = %d MiB, want at most %d MiB", hwm>>20, memoryBudget>>20)
	}
	if keys != memShowLibLeaves+memShowLibLeaves/50 || largest != libstats.TopN {
		t.Errorf("child read %d keys and %d largest, want %d and %d", keys, largest, memShowLibLeaves+memShowLibLeaves/50, libstats.TopN)
	}
}
