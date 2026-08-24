// Command spotcheck dumps the raw legs of recorded samples around one
// timestamp — so a single profitable window can be verified end-to-end by
// hand — together with Binance 1s klines covering the same interval.
//
// Example:
//
//	spotcheck -pair AVAX/USDT -at 2026-08-24T12:34:56Z -window 30s
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mister0s/coins-monitor/internal/cex"
	"github.com/mister0s/coins-monitor/internal/config"
	"github.com/mister0s/coins-monitor/internal/engine"
	"github.com/mister0s/coins-monitor/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/monitor.db", "SQLite sample database")
	configPath := flag.String("config", "config.yaml", "config file (resolves CEX symbol/endpoint)")
	pair := flag.String("pair", "", "pair symbol, e.g. AVAX/USDT (required)")
	at := flag.String("at", "", "window timestamp, RFC3339, e.g. 2026-08-24T12:34:56Z (required)")
	window := flag.Duration("window", 30*time.Second, "half-width: samples/klines within ±window of -at")
	flag.Parse()

	if *pair == "" || *at == "" {
		flag.Usage()
		os.Exit(2)
	}
	center, err := time.Parse(time.RFC3339, *at)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad -at timestamp:", err)
		os.Exit(2)
	}
	from, to := center.Add(-*window), center.Add(*window)

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	samples, err := st.SamplesBetween(*pair, from, to)
	st.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	printSamples(samples, center)

	cexSymbol, restEndpoint := resolveCex(*configPath, *pair)
	fmt.Printf("\n== binance 1s klines: %s  %s .. %s ==\n",
		cexSymbol, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err := printKlines(restEndpoint, cexSymbol, from, to); err != nil {
		fmt.Fprintf(os.Stderr, "kline fetch failed (samples above are still valid): %v\n", err)
	}
}

func printSamples(samples []engine.Sample, center time.Time) {
	if len(samples) == 0 {
		fmt.Println("no samples in range — check -pair / -at / -db")
		return
	}
	fmt.Printf("== samples (%d) — > marks rows nearest -at ==\n", len(samples))
	fmt.Printf("%-1s %-12s %8s | %10s %10s | %10s %10s %7s | %7s %7s | %6s %5s %s\n",
		"", "ts(utc)", "size", "cex_bid", "cex_ask", "dex_buy", "dex_sell", "basis",
		"net_raw", "net_adj", "skewms", "mom", "excl")
	var nearest time.Duration = -1
	for _, s := range samples {
		d := s.Ts.Sub(center)
		if d < 0 {
			d = -d
		}
		if nearest < 0 || d < nearest {
			nearest = d
		}
	}
	for _, s := range samples {
		d := s.Ts.Sub(center)
		if d < 0 {
			d = -d
		}
		mark := " "
		if d == nearest {
			mark = ">"
		}
		netRaw := s.NetBuyDexSellCexBps
		netAdj := s.NetBuyDexSellCexAdjBps
		basis := "-"
		if s.BasisMid > 0 {
			basis = fmt.Sprintf("%.5f", s.BasisMid)
		}
		mom := "-"
		if s.MomentumOK {
			mom = fmt.Sprintf("%.1f", s.CexMidChg10sBps)
		}
		fmt.Printf("%-1s %-12s %8.0f | %10.4f %10.4f | %10.4f %10.4f %7s | %7.1f %7.1f | %6d %5s %s\n",
			mark, s.Ts.UTC().Format("15:04:05.000"), s.TradeSizeUSD,
			s.CexBid, s.CexAsk, s.DexBuyPrice, s.DexSellPrice, basis,
			netRaw, netAdj, s.SkewMs, mom, s.ExcludeReason)
	}
	fmt.Println("net_raw/net_adj = buy_dex_sell_cex direction; dex prices are in the DEX quote token.")
}

// resolveCex maps the pair symbol to its exchange symbol and REST endpoint,
// falling back to derivation when the config is unavailable.
func resolveCex(configPath, pair string) (symbol, endpoint string) {
	symbol = strings.ToUpper(strings.ReplaceAll(pair, "/", ""))
	endpoint = "https://api.binance.com"
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: %v — deriving CEX symbol %s\n", err, symbol)
		return
	}
	for _, p := range cfg.Pairs {
		if p.Symbol == pair {
			return p.CEX.Symbol, p.CEX.RestEndpoint
		}
	}
	return
}

func printKlines(endpoint, symbol string, from, to time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := cex.BinanceKlines(ctx, endpoint, symbol, "1s", from, to)
	if err != nil {
		return err
	}
	fmt.Printf("%-12s %12s %12s %12s %12s %12s\n", "open(utc)", "open", "high", "low", "close", "base_vol")
	for _, k := range rows {
		fmt.Printf("%-12s %12.6f %12.6f %12.6f %12.6f %12.4f\n",
			k.OpenTime.Format("15:04:05"), k.Open, k.High, k.Low, k.Close, k.Volume)
	}
	return nil
}
