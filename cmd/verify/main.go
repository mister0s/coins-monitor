// Command verify cross-checks recently recorded samples against an
// independent public source: for each checked sample, the Binance REST kline
// covering the sample's CEX receive time is fetched over a fresh HTTP
// connection (independent of the websocket that produced the sample), and
// the recorded mid must lie inside the kline's [low, high] range.
//
// This validates that stored data reflects what the venue reports for the
// same moment — it detects recording/pipeline corruption and clock problems.
// It cannot detect the venue itself misreporting, but WS and REST are
// separate transports, so agreement is meaningful evidence.
//
// Exit code is non-zero if any deviation exceeds -max-dev-bps.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mister0s/coins-monitor/internal/cex"
	"github.com/mister0s/coins-monitor/internal/config"
	"github.com/mister0s/coins-monitor/internal/engine"
	"github.com/mister0s/coins-monitor/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/monitor.db", "SQLite sample database")
	configPath := flag.String("config", "config.yaml", "config file (resolves venue endpoints)")
	perPair := flag.Int("n", 8, "samples to check per pair")
	lookback := flag.Duration("lookback", time.Hour, "check samples recorded within this window")
	maxDevBps := flag.Float64("max-dev-bps", 20, "fail if any recorded mid deviates from the kline range by more than this")
	flag.Parse()

	if err := run(*dbPath, *configPath, *perPair, *lookback, *maxDevBps); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dbPath, configPath string, perPair int, lookback time.Duration, maxDevBps float64) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	worst := 0.0
	checked, breaches := 0, 0

	for _, p := range cfg.Pairs {
		if !p.IsEnabled() {
			continue
		}
		if p.CEX.Venue != "binance" {
			fmt.Printf("%-10s skipped: kline verification only implemented for binance venues\n", p.Symbol)
			continue
		}
		samples, err := st.SamplesBetween(p.Symbol, now.Add(-lookback), now)
		if err != nil {
			return err
		}
		if len(samples) == 0 {
			fmt.Printf("%-10s no samples in the last %s\n", p.Symbol, lookback)
			continue
		}
		picked := pickSpread(samples, perPair)
		fmt.Printf("\n%s — checking %d of %d samples against %s klines:\n",
			p.Symbol, len(picked), len(samples), p.CEX.RestEndpoint)
		fmt.Printf("  %-25s %12s %12s %12s %10s %s\n", "cex_ts", "recorded_mid", "kline_low", "kline_high", "dev_bps", "conn")
		for _, s := range picked {
			dev, lo, hi, err := checkSample(ctx, p, s)
			if err != nil {
				fmt.Printf("  %-25s kline fetch failed: %v\n", s.CexTs.UTC().Format(time.RFC3339), err)
				continue
			}
			checked++
			flag := ""
			if dev > maxDevBps {
				breaches++
				flag = "  <-- EXCEEDS THRESHOLD"
			}
			if dev > worst {
				worst = dev
			}
			fmt.Printf("  %-25s %12.6f %12.6f %12.6f %10.2f %s%s\n",
				s.CexTs.UTC().Format(time.RFC3339), s.CexMid, lo, hi, dev, s.CexConnID, flag)
		}
	}

	fmt.Printf("\nchecked %d samples; worst deviation %.2f bps; %d above the %.0f bps threshold\n",
		checked, worst, breaches, maxDevBps)
	fmt.Println("dev_bps = 0 means the recorded mid lies inside the venue's [low,high] for that")
	fmt.Println("interval; a nonzero value is the distance to the nearest bound. WS and REST are")
	fmt.Println("independent transports, so agreement indicates the recording pipeline is sound.")
	if checked == 0 {
		return fmt.Errorf("nothing verified — no reachable samples/klines")
	}
	if breaches > 0 {
		return fmt.Errorf("%d samples deviate beyond %.0f bps — inspect with cmd/spotcheck", breaches, maxDevBps)
	}
	return nil
}

// pickSpread selects up to n samples evenly spread across the slice.
func pickSpread(samples []engine.Sample, n int) []engine.Sample {
	if len(samples) <= n {
		return samples
	}
	out := make([]engine.Sample, 0, n)
	step := float64(len(samples)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, samples[int(float64(i)*step)])
	}
	return out
}

// checkSample fetches the kline covering the sample's CEX receive time and
// returns the deviation of the recorded mid from the kline's range in bps.
// 1s klines give the tightest check; it falls back to 1m if unavailable.
func checkSample(ctx context.Context, p config.Pair, s engine.Sample) (dev, lo, hi float64, err error) {
	kctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	at := s.CexTs
	ks, err := cex.BinanceKlines(kctx, p.CEX.RestEndpoint, p.CEX.Symbol, "1s",
		at.Add(-2*time.Second), at.Add(2*time.Second))
	if err != nil || len(ks) == 0 {
		ks, err = cex.BinanceKlines(kctx, p.CEX.RestEndpoint, p.CEX.Symbol, "1m",
			at.Truncate(time.Minute), at.Truncate(time.Minute).Add(time.Minute-time.Millisecond))
		if err != nil {
			return 0, 0, 0, err
		}
	}
	if len(ks) == 0 {
		return 0, 0, 0, fmt.Errorf("no klines returned for %s", at)
	}
	// Envelope over the returned candles around the timestamp.
	lo, hi = ks[0].Low, ks[0].High
	for _, k := range ks[1:] {
		if k.Low < lo {
			lo = k.Low
		}
		if k.High > hi {
			hi = k.High
		}
	}
	switch {
	case s.CexMid < lo:
		dev = (lo/s.CexMid - 1) * 1e4
	case s.CexMid > hi:
		dev = (s.CexMid/hi - 1) * 1e4
	}
	return dev, lo, hi, nil
}
