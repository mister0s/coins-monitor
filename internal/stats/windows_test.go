package stats

import (
	"math"
	"testing"
	"time"
)

// series builds an Agg with samples spaced 2s apart; nets[i] <= 0 marks a
// non-positive sample.
func series(nets []float64) *Agg {
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Tier: "large", Symbol: "T/U", SizeUSD: 1000, Direction: "buy_dex_sell_cex"}}
	for i, n := range nets {
		a.add(base.Add(time.Duration(i)*2*time.Second), n, n)
	}
	return a
}

func TestFindWindows(t *testing.T) {
	cases := []struct {
		name      string
		nets      []float64
		wantCount int
		wantDurs  []float64 // seconds, in order
	}{
		{
			name:      "no positive samples",
			nets:      []float64{-1, -2, -0.5, 0},
			wantCount: 0,
		},
		{
			name: "single positive sample is one window of one gap",
			nets: []float64{-1, 5, -1, -1},
			// duration = (end-start=0) + spacing 2s
			wantCount: 1,
			wantDurs:  []float64{2},
		},
		{
			name:      "run of three spans two gaps plus extension",
			nets:      []float64{-1, 3, 4, 5, -1, -1},
			wantCount: 1,
			wantDurs:  []float64{6}, // 4s observed + 2s extension
		},
		{
			name:      "two separate windows",
			nets:      []float64{2, -1, -1, 7, 8, -1},
			wantCount: 2,
			wantDurs:  []float64{2, 4},
		},
		{
			name:      "positive at both ends",
			nets:      []float64{5, -1, -1, -1, 5},
			wantCount: 2,
			wantDurs:  []float64{2, 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := FindWindows(series(tc.nets))
			if ws.Count != tc.wantCount {
				t.Fatalf("count = %d, want %d", ws.Count, tc.wantCount)
			}
			for i, want := range tc.wantDurs {
				got := ws.Windows[i].Duration.Seconds()
				if math.Abs(got-want) > 1e-9 {
					t.Errorf("window %d duration = %vs, want %vs", i, got, want)
				}
			}
		})
	}
}

func TestFindWindowsBreaksOnDataGap(t *testing.T) {
	// Positive samples at t=0s and t=2s, then a 60s outage, then positive
	// again: the gap (> 3x median spacing) must split the run, never
	// bridging unobserved time into one long "window".
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Symbol: "T/U"}}
	for _, off := range []time.Duration{0, 2 * time.Second, 62 * time.Second, 64 * time.Second} {
		a.add(base.Add(off), 5, 5)
	}
	ws := FindWindows(a)
	if ws.Count != 2 {
		t.Fatalf("count = %d, want 2 (outage must split the window)", ws.Count)
	}
	if ws.Windows[0].Duration > 10*time.Second || ws.Windows[1].Duration > 10*time.Second {
		t.Errorf("windows must not span the outage: %v, %v",
			ws.Windows[0].Duration, ws.Windows[1].Duration)
	}
}

func TestEdgeWeightedDuration(t *testing.T) {
	// One long marginal window (6s at ~1bps) and one short fat one (2s at
	// ~50bps): the edge-weighted mean must sit far closer to the short
	// window's duration than the plain mean (4s) does.
	nets := []float64{1, 1, 1, -1, 50, -1}
	ws := FindWindows(series(nets))
	if ws.Count != 2 {
		t.Fatalf("count = %d, want 2", ws.Count)
	}
	// windows: 6s @ 1bps and 2s @ 50bps → ew = (6*1 + 2*50)/51 ≈ 2.08s
	if ws.EWDurS > 3.0 {
		t.Errorf("edge-weighted duration = %.2fs; the fat 2s window should dominate", ws.EWDurS)
	}
	plainMean := (ws.Windows[0].Duration.Seconds() + ws.Windows[1].Duration.Seconds()) / 2
	if ws.EWDurS >= plainMean {
		t.Errorf("ew duration (%.2f) should be below plain mean (%.2f)", ws.EWDurS, plainMean)
	}
}
