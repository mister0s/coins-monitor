// Package engine wires CEX feeds and DEX quoters together, samples spreads on
// an interval, and records observations. It never signs, sends, or executes
// anything: monitoring only.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mister0s/coins-monitor/internal/cex"
	"github.com/mister0s/coins-monitor/internal/config"
	"github.com/mister0s/coins-monitor/internal/dex"
)

// PairRunner samples one configured pair.
type PairRunner struct {
	Pair   config.Pair
	Feed   cex.Feed
	Quoter dex.Quoter

	cfg     *config.Config
	log     *slog.Logger
	w       *JSONLWriter
	stagger time.Duration // initial offset so pairs on one endpoint interleave

	mu          sync.Mutex
	tvlUSD      float64 // -1 = unsupported/unknown
	tvlAt       time.Time
	lastBestNet float64
	lastSample  time.Time
}

// Engine drives all pair runners.
type Engine struct {
	cfg     *config.Config
	log     *slog.Logger
	w       *JSONLWriter
	runners []*PairRunner
}

func New(cfg *config.Config, log *slog.Logger, w *JSONLWriter, runners []*PairRunner) *Engine {
	// Spread the start of pairs that share a chain endpoint across the
	// sampling interval, so their bursts interleave instead of colliding.
	// The shared per-chain rate limiter is the hard cap; this just smooths.
	perChainIdx := map[string]int{}
	perChainTotal := map[string]int{}
	for _, r := range runners {
		perChainTotal[r.Pair.DEX.Chain]++
	}
	for _, r := range runners {
		r.cfg = cfg
		r.log = log.With("pair", r.Pair.Symbol)
		r.w = w
		r.tvlUSD = -1
		chain := r.Pair.DEX.Chain
		n := perChainTotal[chain]
		if n > 1 {
			interval := r.Pair.EffectiveInterval(cfg.PollInterval).Std()
			r.stagger = time.Duration(perChainIdx[chain]) * interval / time.Duration(n)
		}
		perChainIdx[chain]++
	}
	return &Engine{cfg: cfg, log: log, w: w, runners: runners}
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range e.runners {
		wg.Add(1)
		go func(r *PairRunner) {
			defer wg.Done()
			r.run(ctx)
		}(r)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.summaryLoop(ctx)
	}()
	wg.Wait()
}

func (e *Engine) summaryLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.SummaryInterval.Std())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, r := range e.runners {
			r.mu.Lock()
			last, best, tvl := r.lastSample, r.lastBestNet, r.tvlUSD
			r.mu.Unlock()
			if last.IsZero() {
				e.log.Info("summary: no samples yet", "pair", r.Pair.Symbol)
				continue
			}
			e.log.Info("summary",
				"pair", r.Pair.Symbol,
				"tier", r.Pair.Tier,
				"best_net_bps", fmt.Sprintf("%.1f", best),
				"pool_tvl_usd", fmt.Sprintf("%.0f", tvl),
				"last_sample_age", time.Since(last).Round(time.Second),
			)
		}
	}
}

func (r *PairRunner) run(ctx context.Context) {
	if r.stagger > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.stagger):
		}
	}
	interval := r.Pair.EffectiveInterval(r.cfg.PollInterval).Std()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.sampleOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *PairRunner) sampleOnce(ctx context.Context) {
	// A first book read seeds the TVL refresh and sanity checks; each trade
	// size then re-reads the cached book right before quoting to keep the
	// CEX leg as fresh as possible.
	book, ok := r.freshBook()
	if !ok {
		return
	}
	r.refreshTVL(ctx, book.Mid())
	r.mu.Lock()
	tvl := r.tvlUSD
	r.mu.Unlock()
	tvlOK := tvl < 0 || r.Pair.MinPoolTVLUSD <= 0 || tvl >= r.Pair.MinPoolTVLUSD

	gasUSD := r.cfg.Costs.GasUSD[r.Pair.DEX.Chain]
	bestNet := 0.0
	haveBest := false

	for _, size := range r.Pair.TradeSizesUSD {
		book, ok := r.freshBook()
		if !ok {
			return
		}
		mid := book.Mid()

		qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		baseOut, errBuy := r.Quoter.QuoteBuyBase(qctx, size)
		quoteOut, errSell := r.Quoter.QuoteSellBase(qctx, size/mid)
		cancel()
		dexTs := time.Now()
		if ctx.Err() != nil {
			return
		}
		if errBuy != nil || errSell != nil {
			r.log.Warn("DEX quote failed", "size_usd", size, "buy_err", errBuy, "sell_err", errSell)
			continue
		}
		if baseOut <= 0 || quoteOut <= 0 {
			continue
		}

		skew := dexTs.Sub(book.Ts)
		if skew < 0 {
			skew = -skew
		}
		maxSkew := r.cfg.MaxLegSkew.Std()
		skewOK := maxSkew <= 0 || skew <= maxSkew
		var reasons []string
		if !tvlOK {
			reasons = append(reasons, "tvl")
		}
		if !skewOK {
			reasons = append(reasons, "skew")
		}

		s := Sample{
			Ts:             dexTs.UTC(),
			Symbol:         r.Pair.Symbol,
			Tier:           r.Pair.Tier,
			CexVenue:       r.Feed.Venue(),
			DexVenue:       r.Quoter.Venue(),
			Chain:          r.Pair.DEX.Chain,
			TradeSizeUSD:   size,
			CexTs:          book.Ts.UTC(),
			DexTs:          dexTs.UTC(),
			SkewMs:         skew.Milliseconds(),
			CexBid:         book.Bid,
			CexAsk:         book.Ask,
			CexMid:         mid,
			DexBuyPrice:    size / baseOut,
			DexSellPrice:   quoteOut / (size / mid),
			CexFeeBps:      r.cfg.Costs.CexTakerFeeBps,
			GasUSD:         gasUSD,
			BufferBps:      r.cfg.Costs.ExtraBufferBps,
			PoolTVLUSD:     tvl,
			MinPoolTVLUSD:  r.Pair.MinPoolTVLUSD,
			IncludeInStats: len(reasons) == 0,
			ExcludeReason:  strings.Join(reasons, "+"),
		}
		s.computeEdges()
		if err := r.w.Write(s); err != nil {
			r.log.Error("failed to record sample", "err", err)
		}
		net := s.NetBuyDexSellCexBps
		if s.NetBuyCexSellDexBps > net {
			net = s.NetBuyCexSellDexBps
		}
		if !haveBest || net > bestNet {
			bestNet, haveBest = net, true
		}
		if net > 0 && s.IncludeInStats {
			dir := "buy_dex_sell_cex"
			if s.NetBuyCexSellDexBps > s.NetBuyDexSellCexBps {
				dir = "buy_cex_sell_dex"
			}
			r.log.Info("POSITIVE net edge observed",
				"size_usd", size, "direction", dir,
				"net_bps", fmt.Sprintf("%.1f", net))
		}
	}
	if haveBest {
		r.mu.Lock()
		r.lastBestNet = bestNet
		r.lastSample = time.Now()
		r.mu.Unlock()
	}
}

// freshBook returns the current cached top-of-book if it is usable.
func (r *PairRunner) freshBook() (cex.Book, bool) {
	book, ok := r.Feed.Book(r.Pair.CEX.Symbol)
	if !ok {
		r.log.Debug("no CEX book yet")
		return cex.Book{}, false
	}
	if age := time.Since(book.Ts); age > r.cfg.MaxBookAge.Std() {
		r.log.Warn("CEX book stale; skipping sample", "age", age.Round(time.Millisecond))
		return cex.Book{}, false
	}
	if book.Mid() <= 0 || book.Bid > book.Ask*1.5 {
		r.log.Warn("implausible CEX book; skipping", "bid", book.Bid, "ask", book.Ask)
		return cex.Book{}, false
	}
	return book, true
}

func (r *PairRunner) refreshTVL(ctx context.Context, basePriceUSD float64) {
	r.mu.Lock()
	fresh := !r.tvlAt.IsZero() && time.Since(r.tvlAt) < r.cfg.TVLRefreshInterval.Std()
	r.mu.Unlock()
	if fresh {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tvl, err := r.Quoter.PoolTVLUSD(tctx, basePriceUSD)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case errors.Is(err, dex.ErrTVLUnsupported):
		r.tvlUSD, r.tvlAt = -1, now
	case err != nil:
		// Keep the previous value but back off before retrying.
		r.tvlAt = now
		r.log.Warn("TVL refresh failed", "err", err)
	default:
		r.tvlUSD, r.tvlAt = tvl, now
	}
}
