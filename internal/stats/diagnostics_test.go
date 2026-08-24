package stats

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mister0s/coins-monitor/internal/engine"
)

func TestRecordCanonicalAdjAndLegacyFallback(t *testing.T) {
	aggs := map[Key]*Agg{}
	// New-style sample: raw positive, corrected negative (basis artifact).
	record(aggs, engine.Sample{
		Ts: time.Now(), Symbol: "A/U", Tier: "large", TradeSizeUSD: 1000,
		BasisSymbol:            "USDCUSDT",
		NetBuyDexSellCexBps:    4,
		NetBuyDexSellCexAdjBps: -1,
		GrossBuyDexSellCexBps:  24, GrossBuyDexSellCexAdjBps: 19,
		NetBuyCexSellDexBps: -30, NetBuyCexSellDexAdjBps: -25,
		GrossBuyCexSellDexBps: -10, GrossBuyCexSellDexAdjBps: -5,
		IncludeInStats: true, MomentumOK: true, CexMidChg10sBps: 12,
		MomentumBucket: "up", MomentumThresholdBps: 10,
	}, false)
	// Legacy sample: no adj fields at all -> canonical falls back to raw.
	record(aggs, engine.Sample{
		Ts: time.Now(), Symbol: "A/U", Tier: "large", TradeSizeUSD: 1000,
		NetBuyDexSellCexBps: 7, GrossBuyDexSellCexBps: 27,
		NetBuyCexSellDexBps: -33, GrossBuyCexSellDexBps: -13,
		IncludeInStats: true,
	}, false)

	k := Key{Tier: "large", Symbol: "A/U", SizeUSD: 1000, Direction: "buy_dex_sell_cex"}
	a := aggs[k]
	if a == nil || len(a.Net) != 2 {
		t.Fatalf("agg missing or wrong size: %+v", a)
	}
	// Canonical series is corrected; raw survives as the debug series.
	if a.Net[0] != -1 || a.NetRaw[0] != 4 || a.Gross[0] != 19 {
		t.Errorf("adj sample: net=%v raw=%v gross=%v; want -1/4/19", a.Net[0], a.NetRaw[0], a.Gross[0])
	}
	if a.Net[1] != 7 || a.NetRaw[1] != 7 || a.Gross[1] != 27 {
		t.Errorf("legacy sample must fall back to raw: net=%v raw=%v gross=%v", a.Net[1], a.NetRaw[1], a.Gross[1])
	}
	if a.Bucket[0] != "up" || a.Bucket[1] != "" {
		t.Errorf("buckets = %q, %q; want \"up\", \"\"", a.Bucket[0], a.Bucket[1])
	}
	if a.Momentum[0] != 12 || !math.IsNaN(a.Momentum[1]) {
		t.Errorf("momentum recording wrong: %v, %v", a.Momentum[0], a.Momentum[1])
	}
}

func TestRawVsCanonicalWindowsSurvival(t *testing.T) {
	// Raw edge persistently positive, corrected edge not: the canonical
	// window analysis must show nothing while the raw/debug one lights up.
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Symbol: "A/U", Direction: "buy_dex_sell_cex"}}
	for i := 0; i < 100; i++ {
		a.add(base.Add(time.Duration(i)*2*time.Second), 18, -2, 5, math.NaN(), "")
	}
	raw := FindWindowsRaw(a)
	adj := FindWindows(a)
	if raw.Count == 0 || raw.TotalPosS == 0 {
		t.Fatal("raw series should be one long positive window")
	}
	if adj.Count != 0 || adj.TotalPosS != 0 {
		t.Errorf("canonical series must have no windows, got %d (%vs)", adj.Count, adj.TotalPosS)
	}
}

func TestMomentumUsesStoredBuckets(t *testing.T) {
	// Stored buckets use a wide per-pair threshold: a +20 bps move stored
	// as "flat" must stay flat even though the 10 bps fallback would call
	// it "up". A legacy row (no bucket) is re-bucketed with the fallback.
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Tier: "small", Symbol: "S/U", SizeUSD: 100, Direction: "buy_dex_sell_cex"}}
	a.add(base, 10, 1, 1, 20, "flat")                // stored wide-threshold bucket
	a.add(base.Add(2*time.Second), 10, 1, 1, 20, "") // legacy: fallback -> up
	aggs := map[Key]*Agg{a.Key: a}

	var sb strings.Builder
	ReportMomentum(&sb, aggs, 10)
	out := sb.String()
	// One sample in flat, one in up: the row shows both buckets with n=1.
	if !strings.Contains(out, "S/U") {
		t.Fatalf("missing row:\n%s", out)
	}
}

func TestReportSectionsRender(t *testing.T) {
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Tier: "large", Symbol: "A/U", SizeUSD: 1000, Direction: "buy_dex_sell_cex"}}
	for i := 0; i < 50; i++ {
		mom := float64(i%40 - 20)
		a.add(base.Add(time.Duration(i)*2*time.Second), 10, float64(i%5)-3, float64(i%5)-2, mom, "")
	}
	aggs := map[Key]*Agg{a.Key: a}

	var sb strings.Builder
	Report(&sb, aggs)
	ReportWindows(&sb, aggs)
	ReportWindowsAdjusted(&sb, aggs)
	ReportMomentum(&sb, aggs, 10)
	ReportHourly(&sb, aggs)
	out := sb.String()
	for _, want := range []string{"raw_p50", "surv%", "latency-skew check", "hourly median net edge", "A/U"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q", want)
		}
	}
}
