package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNilLimiterNeverWaits(t *testing.T) {
	var l *Limiter
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Error("nil limiter should be free")
	}
}

func TestLimiterPacesConcurrentCallers(t *testing.T) {
	l := New(100) // 10ms spacing
	const n = 20
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Wait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// 20 calls at 100 rps: the last slot is ~190ms out.
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("20 calls at 100rps finished in %v; want >= ~190ms", elapsed)
	}
}

func TestWaitHonorsContext(t *testing.T) {
	l := New(0.1)                    // 10s spacing
	_ = l.Wait(context.Background()) // consume the immediate slot
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Error("expected context deadline error")
	}
}
