package store

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mister0s/coins-monitor/internal/engine"
)

func sample(ts time.Time, symbol string, size float64) engine.Sample {
	return engine.Sample{
		Ts: ts, Symbol: symbol, Tier: "small", CexVenue: "binance", DexVenue: "uniswap_v2",
		Chain: "ethereum", TradeSizeUSD: size,
		CexTs: ts.Add(-200 * time.Millisecond), DexTs: ts, SkewMs: 200,
		CexBid: 1.001, CexAsk: 1.002, CexMid: 1.0015,
		DexBuyPrice: 1.0, DexSellPrice: 0.999,
		GrossBuyDexSellCexBps: 10, NetBuyDexSellCexBps: -10,
		GrossBuyCexSellDexBps: -30, NetBuyCexSellDexBps: -50,
		BasisSymbol: "USDCUSDT", BasisMid: 1.0003,
		GrossBuyDexSellCexAdjBps: 7, NetBuyDexSellCexAdjBps: -13,
		GrossBuyCexSellDexAdjBps: -27, NetBuyCexSellDexAdjBps: -47,
		CexMidChg10sBps: 4.5, MomentumOK: true, MomentumBucket: "flat", MomentumThresholdBps: 30,
		CexFeeBps: 10, GasUSD: 4, BufferBps: 5,
		PoolTVLUSD: 250000, MinPoolTVLUSD: 200000,
		IncludeInStats: true,
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	want := sample(base, "GLM/USDT", 100)
	want.ExcludeReason = ""
	if err := st.Write(want); err != nil {
		t.Fatal(err)
	}
	// Same identity again: must be ignored, not duplicated.
	if err := st.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(sample(base.Add(2*time.Second), "GLM/USDT", 100)); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Count(); n != 2 {
		t.Fatalf("count = %d, want 2 (duplicate ignored)", n)
	}

	var got []engine.Sample
	if err := st.ForEach(func(s engine.Sample) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d samples", len(got))
	}
	g := got[0]
	if !g.Ts.Equal(want.Ts) || g.Symbol != want.Symbol || g.NetBuyDexSellCexAdjBps != want.NetBuyDexSellCexAdjBps ||
		g.BasisMid != want.BasisMid || g.MomentumBucket != want.MomentumBucket ||
		g.MomentumThresholdBps != want.MomentumThresholdBps || !g.MomentumOK || !g.IncludeInStats ||
		!g.CexTs.Equal(want.CexTs) || g.SkewMs != want.SkewMs {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", g, want)
	}

	rng, err := st.SamplesBetween("GLM/USDT", base.Add(time.Second), base.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rng) != 1 || !rng[0].Ts.Equal(base.Add(2*time.Second)) {
		t.Errorf("SamplesBetween returned %d rows", len(rng))
	}
}

func TestRetainArchivesAndDeletes(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base := time.Now().Add(-40 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		if err := st.Write(sample(base.Add(time.Duration(i)*time.Second), "OLD/USDT", 100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Write(sample(time.Now(), "NEW/USDT", 100)); err != nil {
		t.Fatal(err)
	}

	archiveDir := filepath.Join(dir, "archive")
	n, path, err := st.Retain(time.Now().Add(-30*24*time.Hour), archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("archived %d rows, want 5", n)
	}
	if left, _ := st.Count(); left != 1 {
		t.Errorf("rows left = %d, want 1", left)
	}

	// The archive must contain the 5 old rows as gzip JSONL.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		var s engine.Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		if s.Symbol != "OLD/USDT" {
			t.Errorf("archived wrong symbol %s", s.Symbol)
		}
		count++
	}
	if count != 5 {
		t.Errorf("archive holds %d rows, want 5", count)
	}

	// Nothing left to archive: no new file, no error.
	n, path2, err := st.Retain(time.Now().Add(-30*24*time.Hour), archiveDir)
	if err != nil || n != 0 || path2 != "" {
		t.Errorf("idempotent retain: n=%d path=%q err=%v", n, path2, err)
	}
}
