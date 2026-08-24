// Command monitor samples CEX-DEX arbitrage spreads for the configured pairs
// and appends observations to JSONL files. Strictly read-only market data:
// no order execution, no keys, no wallets.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mister0s/coins-monitor/internal/cex"
	"github.com/mister0s/coins-monitor/internal/config"
	"github.com/mister0s/coins-monitor/internal/dex"
	"github.com/mister0s/coins-monitor/internal/engine"
	"github.com/mister0s/coins-monitor/internal/evm"
	"github.com/mister0s/coins-monitor/internal/ratelimit"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	logLevel := flag.String("log-level", "info", "debug|info|warn|error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One rate limiter per endpoint key (chain name; "solana" covers the
	// Jupiter quote API): every pair on the endpoint shares it, which caps
	// the request rate and interleaves concurrent pollers.
	limiters := map[string]*ratelimit.Limiter{}
	limiterFor := func(key string) *ratelimit.Limiter {
		if l, ok := limiters[key]; ok {
			return l
		}
		l := ratelimit.New(cfg.RateLimitFor(key))
		limiters[key] = l
		return l
	}

	clients := map[string]*evm.Client{} // chain -> client
	clientFor := func(chain string) (*evm.Client, error) {
		if c, ok := clients[chain]; ok {
			return c, nil
		}
		rpcURL, ok := cfg.Chains[chain]
		if !ok || rpcURL == "" {
			return nil, fmt.Errorf("no RPC URL configured for chain %q (set chains.%s, env-expandable)", chain, chain)
		}
		c := evm.NewClient(rpcURL, limiterFor(chain))
		clients[chain] = c
		return c, nil
	}

	// Build quoters first; a pair whose DEX side cannot be constructed is
	// skipped with a warning rather than failing the whole monitor.
	type candidate struct {
		pair   config.Pair
		quoter dex.Quoter
	}
	var candidates []candidate
	for _, p := range cfg.Pairs {
		if !p.IsEnabled() {
			log.Info("pair disabled in config; skipping", "pair", p.Symbol)
			continue
		}
		q, err := buildQuoter(ctx, p, clientFor, limiterFor)
		if err != nil {
			log.Warn("pair skipped: DEX side not usable", "pair", p.Symbol, "err", err)
			continue
		}
		// Probe quote at the smallest size to surface bad pool addresses or
		// fee tiers at startup instead of as recurring runtime errors.
		pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
		_, perr := q.QuoteBuyBase(pctx, p.TradeSizesUSD[0])
		pcancel()
		if perr != nil {
			log.Warn("pair skipped: DEX probe quote failed (check pool address / fee tier / route)",
				"pair", p.Symbol, "err", perr)
			continue
		}
		candidates = append(candidates, candidate{pair: p, quoter: q})
	}

	// Validate CEX listings ("confirm listing"), then build one feed per venue.
	binanceSymbols, kucoinSymbols := []string{}, []string{}
	var runners []*engine.PairRunner
	feeds := map[string]cex.Feed{}
	for _, c := range candidates {
		probe, err := probeFeed(c.pair, cfg, log)
		if err != nil {
			return err
		}
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = probe.ValidateSymbol(vctx, c.pair.CEX.Symbol)
		cancel()
		if err != nil {
			log.Warn("pair skipped: CEX symbol not confirmed", "pair", c.pair.Symbol,
				"venue", c.pair.CEX.Venue, "cex_symbol", c.pair.CEX.Symbol, "err", err)
			continue
		}
		switch c.pair.CEX.Venue {
		case "binance":
			binanceSymbols = append(binanceSymbols, c.pair.CEX.Symbol)
		case "kucoin":
			kucoinSymbols = append(kucoinSymbols, c.pair.CEX.Symbol)
		}
		runners = append(runners, &engine.PairRunner{Pair: c.pair, Quoter: c.quoter})
	}
	if len(runners) == 0 {
		return fmt.Errorf("no usable pairs after validation; check config and logs above")
	}

	// All binance pairs share endpoints in practice; use the first pair's.
	for _, r := range runners {
		switch r.Pair.CEX.Venue {
		case "binance":
			f, ok := feeds["binance"]
			if !ok {
				f = cex.NewBinanceFeed(r.Pair.CEX.WSEndpoint, r.Pair.CEX.RestEndpoint, binanceSymbols, log)
				feeds["binance"] = f
			}
			r.Feed = f
		case "kucoin":
			f, ok := feeds["kucoin"]
			if !ok {
				f = cex.NewKucoinFeed(r.Pair.CEX.RestEndpoint, kucoinSymbols, cfg.PollInterval.Std()/3, log)
				feeds["kucoin"] = f
			}
			r.Feed = f
		}
	}

	w, err := engine.NewJSONLWriter(cfg.DataDir)
	if err != nil {
		return err
	}
	defer w.Close()

	for _, f := range feeds {
		f.Start(ctx)
	}
	log.Info("monitor started",
		"pairs", len(runners),
		"poll_interval", cfg.PollInterval.Std(),
		"data_dir", cfg.DataDir,
	)
	for _, r := range runners {
		log.Info("monitoring",
			"pair", r.Pair.Symbol, "tier", r.Pair.Tier,
			"cex", r.Pair.CEX.Venue, "dex", r.Quoter.Venue(), "chain", r.Pair.DEX.Chain,
			"sizes_usd", r.Pair.TradeSizesUSD)
	}

	engine.New(cfg, log, w, runners).Run(ctx)
	log.Info("monitor stopped")
	return nil
}

// probeFeed returns a symbol-less feed used only for listing validation.
func probeFeed(p config.Pair, cfg *config.Config, log *slog.Logger) (cex.Feed, error) {
	switch p.CEX.Venue {
	case "binance":
		return cex.NewBinanceFeed(p.CEX.WSEndpoint, p.CEX.RestEndpoint, nil, log), nil
	case "kucoin":
		return cex.NewKucoinFeed(p.CEX.RestEndpoint, nil, cfg.PollInterval.Std(), log), nil
	default:
		return nil, fmt.Errorf("unsupported cex venue %q", p.CEX.Venue)
	}
}

func buildQuoter(ctx context.Context, p config.Pair, clientFor func(string) (*evm.Client, error), limiterFor func(string) *ratelimit.Limiter) (dex.Quoter, error) {
	d := p.DEX
	if d.Venue == "jupiter" {
		return dex.NewJupiter(d.QuoteURL, d.BaseMint, d.BaseToken.Decimals, d.QuoteMint, d.QuoteToken.Decimals, limiterFor(d.Chain))
	}

	client, err := clientFor(d.Chain)
	if err != nil {
		return nil, err
	}
	baseDec, err := resolveDecimals(ctx, client, d.BaseToken, "base_token")
	if err != nil {
		return nil, err
	}
	quoteDec, err := resolveDecimals(ctx, client, d.QuoteToken, "quote_token")
	if err != nil {
		return nil, err
	}

	switch d.Venue {
	case "uniswap_v3":
		route := make([]dex.PathHop, 0, len(d.Route))
		for _, h := range d.Route {
			route = append(route, dex.PathHop{Token: h.Token, FeeTier: h.FeeTier})
		}
		return dex.NewUniswapV3(client, d.QuoterAddress, d.PoolAddress,
			d.BaseToken.Address, baseDec, d.QuoteToken.Address, quoteDec, d.FeeTier, route)
	case "lfj":
		ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return dex.NewLFJ(ictx, client, d.PoolAddress, d.BaseToken.Address, baseDec, d.QuoteToken.Address, quoteDec)
	case "velodrome":
		ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return dex.NewVelodrome(ictx, client, d.PoolAddress, d.BaseToken.Address, baseDec, d.QuoteToken.Address, quoteDec)
	default:
		return nil, fmt.Errorf("unsupported dex venue %q", d.Venue)
	}
}

func resolveDecimals(ctx context.Context, client *evm.Client, t config.Token, which string) (int, error) {
	if t.Address == "" {
		return 0, fmt.Errorf("%s.address is required", which)
	}
	if t.Decimals > 0 {
		return t.Decimals, nil
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d, err := client.ERC20Decimals(dctx, t.Address)
	if err != nil {
		return 0, fmt.Errorf("fetch %s decimals: %w", which, err)
	}
	return d, nil
}
