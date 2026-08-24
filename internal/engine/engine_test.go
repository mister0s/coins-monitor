package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mister0s/coins-monitor/internal/cex"
	"github.com/mister0s/coins-monitor/internal/config"
)

type fakeFeed struct{ book cex.Book }

func (f *fakeFeed) Book(string) (cex.Book, bool)                 { return f.book, true }
func (f *fakeFeed) ValidateSymbol(context.Context, string) error { return nil }
func (f *fakeFeed) Start(context.Context)                        {}
func (f *fakeFeed) Venue() string                                { return "fake_cex" }

// fakeQuoter simulates a constant-price DEX with a fixed half-spread.
type fakeQuoter struct {
	price  float64 // mid
	skew   float64 // multiplicative penalty per side, e.g. 0.001
	tvl    float64
	tvlErr error
}

func (q *fakeQuoter) QuoteBuyBase(_ context.Context, quoteIn float64) (float64, error) {
	return quoteIn / (q.price * (1 + q.skew)), nil
}
func (q *fakeQuoter) QuoteSellBase(_ context.Context, baseIn float64) (float64, error) {
	return baseIn * q.price * (1 - q.skew), nil
}
func (q *fakeQuoter) PoolTVLUSD(context.Context, float64) (float64, error) {
	return q.tvl, q.tvlErr
}
func (q *fakeQuoter) Venue() string { return "fake_dex" }

func testConfig(t *testing.T) *config.Config {
	return &config.Config{
		PollInterval:       config.Duration(time.Hour),
		TVLRefreshInterval: config.Duration(time.Hour),
		SummaryInterval:    config.Duration(time.Hour),
		MaxBookAge:         config.Duration(10 * time.Second),
		DataDir:            t.TempDir(),
		Costs: config.Costs{
			CexTakerFeeBps: 10,
			ExtraBufferBps: 5,
			GasUSD:         map[string]float64{"testchain": 1},
		},
	}
}

func readSamples(t *testing.T, dir, name string) []Sample {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestSampleOnceEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	w, err := NewJSONLWriter(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// DEX trades 1% below CEX: buying on DEX and selling on CEX should be
	// clearly profitable net of 10+5 bps fees + $1 gas.
	pair := config.Pair{
		Symbol: "TST/USDT", Tier: "small",
		CEX:           config.CexConfig{Venue: "binance", Symbol: "TSTUSDT"},
		DEX:           config.DexConfig{Chain: "testchain", Venue: "uniswap_v3"},
		TradeSizesUSD: []float64{1000, 5000},
		MinPoolTVLUSD: 200000,
	}
	r := &PairRunner{
		Pair:   pair,
		Feed:   &fakeFeed{book: cex.Book{Bid: 99.9, Ask: 100.1, Ts: time.Now()}},
		Quoter: &fakeQuoter{price: 99.0, skew: 0.0005, tvl: 500000},
	}
	New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), w, []*PairRunner{r})

	r.sampleOnce(context.Background())

	samples := readSamples(t, cfg.DataDir, "TST-USDT.jsonl")
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2 (one per trade size)", len(samples))
	}
	for _, s := range samples {
		if !s.IncludeInStats {
			t.Error("TVL 500k >= 200k gate: sample should be included")
		}
		if s.NetBuyDexSellCexBps <= 0 {
			t.Errorf("size %v: expected positive net edge buying cheap DEX, got %v",
				s.TradeSizeUSD, s.NetBuyDexSellCexBps)
		}
		if s.NetBuyCexSellDexBps >= 0 {
			t.Errorf("size %v: expected negative net edge in losing direction, got %v",
				s.TradeSizeUSD, s.NetBuyCexSellDexBps)
		}
		if s.DexBuyPrice <= 99.0 || s.DexSellPrice >= 99.0 {
			t.Errorf("effective prices must straddle DEX mid: buy=%v sell=%v", s.DexBuyPrice, s.DexSellPrice)
		}
	}
	// Larger size pays less fixed gas in bps, so its net must be higher here
	// (fake quoter has size-independent impact).
	if samples[1].NetBuyDexSellCexBps <= samples[0].NetBuyDexSellCexBps {
		t.Errorf("gas amortization: net at $5000 (%v) should exceed net at $1000 (%v)",
			samples[1].NetBuyDexSellCexBps, samples[0].NetBuyDexSellCexBps)
	}
}

func TestTVLGateExcludes(t *testing.T) {
	cfg := testConfig(t)
	w, _ := NewJSONLWriter(cfg.DataDir)
	defer w.Close()
	pair := config.Pair{
		Symbol: "THN/USDT", Tier: "small",
		CEX:           config.CexConfig{Venue: "binance", Symbol: "THNUSDT"},
		DEX:           config.DexConfig{Chain: "testchain", Venue: "uniswap_v3"},
		TradeSizesUSD: []float64{100},
		MinPoolTVLUSD: 200000,
	}
	r := &PairRunner{
		Pair:   pair,
		Feed:   &fakeFeed{book: cex.Book{Bid: 1.0, Ask: 1.001, Ts: time.Now()}},
		Quoter: &fakeQuoter{price: 1.0, skew: 0.001, tvl: 50000}, // below the gate
	}
	New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), w, []*PairRunner{r})
	r.sampleOnce(context.Background())

	samples := readSamples(t, cfg.DataDir, "THN-USDT.jsonl")
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(samples))
	}
	if samples[0].IncludeInStats {
		t.Error("sample below min_pool_tvl_usd must be flagged excluded")
	}
	if samples[0].PoolTVLUSD != 50000 {
		t.Errorf("recorded TVL = %v", samples[0].PoolTVLUSD)
	}
}

func TestSkewGateExcludes(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxLegSkew = config.Duration(3 * time.Second)
	w, _ := NewJSONLWriter(cfg.DataDir)
	defer w.Close()
	pair := config.Pair{
		Symbol: "SKW/USDT", Tier: "large",
		CEX:           config.CexConfig{Venue: "binance", Symbol: "SKWUSDT"},
		DEX:           config.DexConfig{Chain: "testchain", Venue: "uniswap_v3"},
		TradeSizesUSD: []float64{1000},
	}
	// Book is 5s old: fresh enough for MaxBookAge (10s) but the DEX quote
	// completes now, so the leg skew (~5s) breaches max_leg_skew (3s).
	r := &PairRunner{
		Pair:   pair,
		Feed:   &fakeFeed{book: cex.Book{Bid: 99.9, Ask: 100.1, Ts: time.Now().Add(-5 * time.Second)}},
		Quoter: &fakeQuoter{price: 99.0, skew: 0.0005, tvl: 500000},
	}
	New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), w, []*PairRunner{r})
	r.sampleOnce(context.Background())

	samples := readSamples(t, cfg.DataDir, "SKW-USDT.jsonl")
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1 (recorded despite skew)", len(samples))
	}
	s := samples[0]
	if s.IncludeInStats {
		t.Error("sample with 5s leg skew must be excluded from stats")
	}
	if s.ExcludeReason != "skew" {
		t.Errorf("exclude_reason = %q, want \"skew\"", s.ExcludeReason)
	}
	if s.SkewMs < 4500 {
		t.Errorf("skew_ms = %d, want ~5000", s.SkewMs)
	}
	if s.CexTs.IsZero() || s.DexTs.IsZero() || !s.DexTs.After(s.CexTs) {
		t.Errorf("leg timestamps not recorded: cex_ts=%v dex_ts=%v", s.CexTs, s.DexTs)
	}
}

func TestStaleBookSkipsSample(t *testing.T) {
	cfg := testConfig(t)
	w, _ := NewJSONLWriter(cfg.DataDir)
	defer w.Close()
	pair := config.Pair{
		Symbol: "OLD/USDT", Tier: "large",
		CEX:           config.CexConfig{Venue: "binance", Symbol: "OLDUSDT"},
		DEX:           config.DexConfig{Chain: "testchain", Venue: "uniswap_v3"},
		TradeSizesUSD: []float64{100},
	}
	r := &PairRunner{
		Pair:   pair,
		Feed:   &fakeFeed{book: cex.Book{Bid: 1, Ask: 1, Ts: time.Now().Add(-time.Minute)}},
		Quoter: &fakeQuoter{price: 1},
	}
	New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), w, []*PairRunner{r})
	r.sampleOnce(context.Background())
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "OLD-USDT.jsonl")); !os.IsNotExist(err) {
		t.Error("stale book must not produce samples")
	}
}
