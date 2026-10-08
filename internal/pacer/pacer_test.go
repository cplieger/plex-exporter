package pacer

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Three loops sharing one pacer never get more than two slots in any one
// second, however their calls interleave.
func TestWait_shared_by_several_loops_stays_under_two_per_second(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(DefaultInterval)
		var mu sync.Mutex
		var got []time.Time
		var wg sync.WaitGroup
		for range 3 {
			wg.Go(func() {
				for range 10 {
					if err := p.Wait(t.Context()); err != nil {
						t.Errorf("Wait() error = %v", err)
						return
					}
					mu.Lock()
					got = append(got, time.Now())
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		for i := range got {
			n := 0
			for _, u := range got {
				if !u.Before(got[i]) && u.Before(got[i].Add(time.Second)) {
					n++
				}
			}
			if n > 2 {
				t.Fatalf("%d requests in the second after %v, want at most 2", n, got[i])
			}
		}
		if len(got) != 30 {
			t.Errorf("granted %d slots, want 30", len(got))
		}
	})
}

func TestWait_returns_when_cancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(time.Hour)
		if err := p.Wait(t.Context()); err != nil {
			t.Fatalf("first Wait() error = %v, want nil", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := p.Wait(ctx); err == nil {
			t.Error("Wait() on a cancelled context = nil, want its error")
		}
	})
}
