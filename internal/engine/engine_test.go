package engine

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mister0s/coins-monitor/internal/cex"
	"github.com/mister0s/coins-monitor/internal/config"
)

type fakeFeed struct {
	book   cex.Book
	books  map[string]cex.Book // per-symbol overrides (e.g. a basis symbol)
	strict bool                // if set, symbols missing from books have no data
}

func (f *fakeFeed) Book(symbol string) (cex.Book, bool) {
	if b, ok := f.books[symbol]; ok {
		return b, true
	}
	if f.strict { // symbols outside the map do not exist
		return cex.Book{}, false
	}
	return f.book, true
}
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
func (q *fakeQuoter) Venue() string  { return "fake_dex" }
func (q *fakeQuoter) Source() string { return "https://fake-rpc.test" }

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

// fakeRecorder captures written samples in memory.
type fakeRecorder struct {
	mu      sync.Mutex
	samples []Sample
}

func (f *fakeRecorder) Write(s Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, s)
	return nil
}

func TestSampleOnceEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	w := &fakeRecorder{}

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

	samples := w.samples
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
	w := &fakeRecorder{}
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

	samples := w.samples
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
	w := &fakeRecorder{}
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

	samples := w.samples
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

func TestQuoteBasisCorrection(t *testing.T) {
	cfg := testConfig(t)
	w := &fakeRecorder{}
	// DEX quoted in USDC while CEX is USDT, USDC trading at 1.0005 USDT:
	// the raw buy-DEX edge overstates reality; the corrected edge must be
	// ~5 bps smaller.
	pair := config.Pair{
		Symbol: "BAS/USDT", Tier: "large",
		CEX: config.CexConfig{Venue: "binance", Symbol: "BASUSDT"},
		DEX: config.DexConfig{
			Chain: "testchain", Venue: "lfj",
			QuoteBasisSymbol: "USDCUSDT",
		},
		TradeSizesUSD: []float64{1000},
	}
	r := &PairRunner{
		Pair: pair,
		Feed: &fakeFeed{
			book: cex.Book{Bid: 100.0, Ask: 100.1, Ts: time.Now()},
			books: map[string]cex.Book{
				"USDCUSDT": {Bid: 1.0004, Ask: 1.0006, Ts: time.Now()},
			},
		},
		Quoter: &fakeQuoter{price: 99.0, skew: 0.0005, tvl: 500000},
	}
	New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), w, []*PairRunner{r})
	r.sampleOnce(context.Background())

	samples := w.samples
	if len(samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(samples))
	}
	s := samples[0]
	if s.BasisSymbol != "USDCUSDT" || s.BasisMid < 1.0004 || s.BasisMid > 1.0006 {
		t.Errorf("basis not recorded: symbol=%q mid=%v", s.BasisSymbol, s.BasisMid)
	}
	shift := s.NetBuyDexSellCexBps - s.NetBuyDexSellCexAdjBps
	if shift < 3 || shift > 7 {
		t.Errorf("corrected buy-DEX edge should be ~5 bps below raw, shift = %.2f", shift)
	}
	// The opposite direction moves the other way.
	if s.NetBuyCexSellDexAdjBps <= s.NetBuyCexSellDexBps {
		t.Errorf("sell-DEX corrected edge should exceed raw: raw=%v adj=%v",
			s.NetBuyCexSellDexBps, s.NetBuyCexSellDexAdjBps)
	}
	// Raw values must be identical to an uncorrected computation.
	var ref Sample
	ref = s
	ref.BasisMid = 0
	ref.computeEdges()
	if ref.NetBuyDexSellCexBps != s.NetBuyDexSellCexBps {
		t.Error("raw fields must not be altered by the correction")
	}
}

func TestMissingBasisExcludesSample(t *testing.T) {
	cfg := testConfig(t)
	w := &fakeRecorder{}
	// Basis symbol configured but its book never arrives: the canonical
	// corrected edge cannot be computed, so the sample must be recorded but
	// excluded — never silently recorded with adj == raw as if corrected.
	pair := config.Pair{
		Symbol: "NOB/USDT", Tier: "large",
		CEX: config.CexConfig{Venue: "binance", Symbol: "NOBUSDT"},
		DEX: config.DexConfig{
			Chain: "testchain", Venue: "lfj",
			QuoteBasisSymbol: "USDCUSDT",
		},
		TradeSizesUSD: []float64{1000},
	}
	r := &PairRunner{
		Pair: pair,
		Feed: &fakeFeed{
			books:  map[string]cex.Book{"NOBUSDT": {Bid: 100.0, Ask: 100.1, Ts: time.Now(), Source: "wss://x", ConnID: "c1"}},
			strict: true, // USDCUSDT missing
		},
		Quoter: &fakeQuoter{price: 99.0, skew: 0.0005, tvl: 500000},
	}
	New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)), w, []*PairRunner{r})
	r.sampleOnce(context.Background())

	if len(w.samples) != 1 {
		t.Fatalf("samples = %d, want 1 (recorded, flagged)", len(w.samples))
	}
	s := w.samples[0]
	if s.IncludeInStats {
		t.Error("sample without its configured basis book must be excluded from stats")
	}
	if s.ExcludeReason != "basis" {
		t.Errorf("exclude_reason = %q, want \"basis\"", s.ExcludeReason)
	}
	if s.BasisMid != 0 {
		t.Errorf("basis_mid must be 0 (unavailable), got %v", s.BasisMid)
	}
	// Provenance recorded from the book and the quoter.
	if s.CexSource != "wss://x" || s.CexConnID != "c1" || s.DexSource != "https://fake-rpc.test" {
		t.Errorf("provenance missing: cex_source=%q conn=%q dex_source=%q",
			s.CexSource, s.CexConnID, s.DexSource)
	}
}

func TestMomentum10s(t *testing.T) {
	r := &PairRunner{}
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if _, ok := r.momentum10s(base, 100); ok {
		t.Error("no history: momentum must be unavailable")
	}
	r.recordMid(base, 100)
	r.recordMid(base.Add(2*time.Second), 100.2)
	if _, ok := r.momentum10s(base.Add(2*time.Second), 100.2); ok {
		t.Error("only 2s of history: momentum must be unavailable")
	}
	now := base.Add(10 * time.Second)
	r.recordMid(now, 101)
	chg, ok := r.momentum10s(now, 101)
	if !ok {
		t.Fatal("10s-old reference exists; momentum must be available")
	}
	if chg < 95 || chg > 105 { // (101/100 - 1)*1e4 = 100 bps
		t.Errorf("momentum = %.1f bps, want ~100", chg)
	}
	// History pruning: nothing older than ~40s survives.
	r.recordMid(base.Add(2*time.Minute), 102)
	if len(r.midHistory) != 1 {
		t.Errorf("history not pruned: %d entries", len(r.midHistory))
	}
}

func TestStaleBookSkipsSample(t *testing.T) {
	cfg := testConfig(t)
	w := &fakeRecorder{}
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
	if len(w.samples) != 0 {
		t.Error("stale book must not produce samples")
	}
}
