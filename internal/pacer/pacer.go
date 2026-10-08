// Package pacer spaces the exporter's background Plex requests so that
// every loop together stays under one request budget per server.
package pacer

import (
	"context"
	"sync"
	"time"
)

// DefaultInterval allows at most 2 background requests per second.
const DefaultInterval = 500 * time.Millisecond

// Pacer hands out request slots at least Interval apart, in call order.
// It is safe for concurrent use.
type Pacer struct {
	next     time.Time
	interval time.Duration
	mu       sync.Mutex
}

// New returns a Pacer whose slots are interval apart.
func New(interval time.Duration) *Pacer {
	return &Pacer{interval: interval}
}

// Wait blocks until the caller's slot, or returns ctx's error. A cancelled
// wait still consumes its slot, which only slows later callers.
func (p *Pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	slot := p.next
	if slot.Before(now) {
		slot = now
	}
	p.next = slot.Add(p.interval)
	p.mu.Unlock()

	d := time.Until(slot)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
