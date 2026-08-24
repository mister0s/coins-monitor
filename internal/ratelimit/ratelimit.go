// Package ratelimit provides a minimal request pacer. One Limiter is shared
// by every consumer of an RPC endpoint (all pairs on a chain), which both
// caps the request rate and naturally staggers concurrent pollers: each
// caller is queued into the next free slot.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type Limiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

// New returns a limiter that admits at most rps requests per second.
// rps <= 0 returns nil, and a nil *Limiter never waits.
func New(rps float64) *Limiter {
	if rps <= 0 {
		return nil
	}
	return &Limiter{interval: time.Duration(float64(time.Second) / rps)}
}

// Wait blocks until the caller's reserved slot arrives or ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	slot := l.next
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()

	d := slot.Sub(now)
	if d <= 0 {
		return nil
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
