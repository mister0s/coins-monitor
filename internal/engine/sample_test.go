package engine

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestComputeEdges(t *testing.T) {
	// CEX at 100.00/100.10; DEX buy costs 99.50/base, DEX sell pays 100.60/base.
	s := Sample{
		TradeSizeUSD: 10000,
		CexBid:       100.00,
		CexAsk:       100.10,
		DexBuyPrice:  99.50,
		DexSellPrice: 100.60,
		CexFeeBps:    10,
		GasUSD:       5, // 5/10000 = 5 bps
		BufferBps:    5,
	}
	s.computeEdges()

	wantGrossA := (100.00/99.50 - 1) * 1e4 // ≈ 50.25 bps
	if !almost(s.GrossBuyDexSellCexBps, wantGrossA) {
		t.Errorf("gross buy-dex = %v, want %v", s.GrossBuyDexSellCexBps, wantGrossA)
	}
	// costs = 10 + 5 + 5 = 20 bps
	if !almost(s.NetBuyDexSellCexBps, wantGrossA-20) {
		t.Errorf("net buy-dex = %v, want %v", s.NetBuyDexSellCexBps, wantGrossA-20)
	}

	wantGrossB := (100.60/100.10 - 1) * 1e4 // ≈ 49.95 bps
	if !almost(s.GrossBuyCexSellDexBps, wantGrossB) {
		t.Errorf("gross buy-cex = %v, want %v", s.GrossBuyCexSellDexBps, wantGrossB)
	}
	if !almost(s.NetBuyCexSellDexBps, wantGrossB-20) {
		t.Errorf("net buy-cex = %v, want %v", s.NetBuyCexSellDexBps, wantGrossB-20)
	}
}

func TestCostBpsScalesWithSize(t *testing.T) {
	// Fixed gas dominates small trades: $4 gas on $100 = 400 bps.
	if got := costBps(10, 4, 5, 100); !almost(got, 415) {
		t.Errorf("costBps small = %v, want 415", got)
	}
	if got := costBps(10, 4, 5, 20000); !almost(got, 17) {
		t.Errorf("costBps large = %v, want 17", got)
	}
}

func TestNoEdgeWithoutProfit(t *testing.T) {
	// Symmetric market: DEX exactly at CEX mid; every direction loses to costs.
	s := Sample{
		TradeSizeUSD: 1000,
		CexBid:       99.95,
		CexAsk:       100.05,
		DexBuyPrice:  100.0,
		DexSellPrice: 100.0,
		CexFeeBps:    10,
	}
	s.computeEdges()
	if s.NetBuyDexSellCexBps >= 0 || s.NetBuyCexSellDexBps >= 0 {
		t.Errorf("expected negative net edges, got %v / %v",
			s.NetBuyDexSellCexBps, s.NetBuyCexSellDexBps)
	}
}
