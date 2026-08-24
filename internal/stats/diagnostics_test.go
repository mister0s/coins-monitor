package stats

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mister0s/coins-monitor/internal/engine"
)

func TestRecordAdjAndLegacyFallback(t *testing.T) {
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
	}, false)
	// Legacy sample: no adj fields at all -> adj must fall back to raw.
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
	if a.NetAdj[0] != -1 {
		t.Errorf("adj sample: NetAdj = %v, want -1", a.NetAdj[0])
	}
	if a.NetAdj[1] != 7 {
		t.Errorf("legacy sample: NetAdj = %v, want raw 7", a.NetAdj[1])
	}
	if a.Momentum[0] != 12 || !math.IsNaN(a.Momentum[1]) {
		t.Errorf("momentum recording wrong: %v, %v", a.Momentum[0], a.Momentum[1])
	}
}

func TestAdjWindowsSurvival(t *testing.T) {
	// Raw edge is persistently positive but the corrected edge is not:
	// FindWindowsAdj must show the "edge" not surviving.
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Symbol: "A/U", Direction: "buy_dex_sell_cex"}}
	for i := 0; i < 100; i++ {
		a.add(base.Add(time.Duration(i)*2*time.Second), 25, 5, -2, math.NaN())
	}
	raw := FindWindows(a)
	adj := FindWindowsAdj(a)
	if raw.Count == 0 || raw.TotalPosS == 0 {
		t.Fatal("raw series should be one long positive window")
	}
	if adj.Count != 0 || adj.TotalPosS != 0 {
		t.Errorf("corrected series must have no windows, got %d (%vs)", adj.Count, adj.TotalPosS)
	}
}

func TestReportSectionsRender(t *testing.T) {
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	a := &Agg{Key: Key{Tier: "large", Symbol: "A/U", SizeUSD: 1000, Direction: "buy_dex_sell_cex"}}
	for i := 0; i < 50; i++ {
		mom := float64(i%40 - 20) // spread across down/flat/up buckets
		a.add(base.Add(time.Duration(i)*2*time.Second), 10, float64(i%5)-2, float64(i%5)-3, mom)
	}
	aggs := map[Key]*Agg{a.Key: a}

	var sb strings.Builder
	ReportWindowsAdjusted(&sb, aggs)
	ReportMomentum(&sb, aggs, 10)
	ReportHourly(&sb, aggs)
	out := sb.String()
	for _, want := range []string{"surv%", "momentum correlation", "hourly median net edge", "A/U"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q", want)
		}
	}
}
