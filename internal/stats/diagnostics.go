package stats

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// ReportWindowsAdjusted contrasts the window analysis on the uncorrected
// (raw/debug) edge with the canonical corrected one and reports how much of
// the raw edge survives. surv% compares total net-positive seconds.
func ReportWindowsAdjusted(w io.Writer, aggs map[Key]*Agg) {
	keys := sortedKeys(aggs)
	fmt.Fprintln(w, "\n== basis-correction survival (raw/debug edge vs canonical corrected edge) ==")
	fmt.Fprintf(w, "%-10s %-9s %-18s | %-24s | %-24s | %6s\n",
		"", "", "", "raw: win  p95s  pos_s/d", "adj: win  p95s  pos_s/d", "surv%")
	fmt.Fprintf(w, "%-10s %-9s %-18s |\n", "pair", "size_usd", "direction")
	fmt.Fprintln(w, strings.Repeat("-", 108))
	currentTier := "\x00"
	for _, k := range keys {
		a := aggs[k]
		if k.Tier != currentTier {
			currentTier = k.Tier
			fmt.Fprintf(w, "== tier: %s ==\n", currentTier)
		}
		raw := FindWindowsRaw(a)
		adj := FindWindows(a)
		span := 0.0
		if len(a.Ts) > 1 {
			tss := append([]time.Time(nil), a.Ts...)
			sort.Slice(tss, func(i, j int) bool { return tss[i].Before(tss[j]) })
			span = tss[len(tss)-1].Sub(tss[0]).Seconds()
		}
		posPerDay := func(ws WindowStats) float64 {
			if span <= 0 {
				return math.NaN()
			}
			return ws.TotalPosS / span * 86400
		}
		surv := math.NaN()
		if raw.TotalPosS > 0 {
			surv = adj.TotalPosS / raw.TotalPosS * 100
		}
		fmt.Fprintf(w, "%-10s %-9.0f %-18s | %8d %6.1f %8.0f | %8d %6.1f %8.0f | %5.1f\n",
			k.Symbol, k.SizeUSD, k.Direction,
			raw.Count, raw.P95s, posPerDay(raw),
			adj.Count, adj.P95s, posPerDay(adj),
			surv)
	}
	fmt.Fprintln(w, `
surv% = corrected net-positive seconds as a share of raw net-positive seconds.
~100% means the edge survives the basis correction; near 0% means the "edge"
was the stablecoin basis (e.g. USDC/USDT deviating from 1), not an arbitrage.
Series without a configured quote_basis_symbol have adj == raw by construction.`)
}

// ReportMomentum is the standing latency-skew check: it buckets samples by
// the CEX mid's move over the prior ~10s and reports the mean canonical net
// edge per bucket. Positive edge concentrated in the strong-up bucket for
// buy_dex_sell_cex (the DEX quote lagging a rising CEX price) indicates
// latency skew rather than genuine opportunity. Buckets come from the
// per-pair threshold stored on each sample; fallbackThresholdBps only
// re-buckets legacy rows recorded before buckets were stored.
func ReportMomentum(w io.Writer, aggs map[Key]*Agg, fallbackThresholdBps float64) {
	keys := sortedKeys(aggs)
	fmt.Fprintf(w, "\n== latency-skew check: net edge by prior-10s CEX momentum (stored per-pair buckets; legacy fallback %.0f bps) ==\n", fallbackThresholdBps)
	fmt.Fprintf(w, "%-10s %-9s %-18s | %-22s | %-22s | %-22s | %6s\n",
		"pair", "size_usd", "direction",
		"down:  n  mean  pos%", "flat:  n  mean  pos%", "up:    n  mean  pos%", "no-mom")
	fmt.Fprintln(w, strings.Repeat("-", 128))
	currentTier := "\x00"
	for _, k := range keys {
		a := aggs[k]
		if k.Tier != currentTier {
			currentTier = k.Tier
			fmt.Fprintf(w, "== tier: %s ==\n", currentTier)
		}
		type bucket struct {
			n, pos int
			sum    float64
		}
		var down, flat, up bucket
		noMom := 0
		for i, m := range a.Momentum {
			if math.IsNaN(m) {
				noMom++
				continue
			}
			name := a.Bucket[i]
			if name == "" { // legacy row: re-bucket with the fallback threshold
				switch {
				case m < -fallbackThresholdBps:
					name = "down"
				case m > fallbackThresholdBps:
					name = "up"
				default:
					name = "flat"
				}
			}
			b := &flat
			switch name {
			case "down":
				b = &down
			case "up":
				b = &up
			}
			b.n++
			b.sum += a.Net[i]
			if a.Net[i] > 0 {
				b.pos++
			}
		}
		cell := func(b bucket) string {
			if b.n == 0 {
				return fmt.Sprintf("%6d %6s %5s", 0, "-", "-")
			}
			return fmt.Sprintf("%6d %6.1f %4.1f%%", b.n, b.sum/float64(b.n),
				float64(b.pos)/float64(b.n)*100)
		}
		fmt.Fprintf(w, "%-10s %-9.0f %-18s | %s | %s | %s | %6d\n",
			k.Symbol, k.SizeUSD, k.Direction, cell(down), cell(flat), cell(up), noMom)
	}
	fmt.Fprintln(w, `
mean = mean basis-corrected net edge (bps) in the bucket; pos% = share of
bucket samples with corrected net edge > 0. If pos% for buy_dex_sell_cex is
concentrated in the "up" bucket (CEX rising into the sample), the DEX quote is
lagging the CEX move — latency skew, not capturable opportunity. no-mom =
samples without a valid 10s momentum reference (e.g. just after startup).`)
}

// ReportHourly prints an hourly time series of the median net edge (raw and
// corrected) for the buy_dex_sell_cex direction. A flat persistent offset
// between raw and corrected points at the quote basis; spiky hours point at
// genuine dislocations.
func ReportHourly(w io.Writer, aggs map[Key]*Agg) {
	keys := sortedKeys(aggs)
	fmt.Fprintln(w, "\n== hourly median net edge, buy_dex_sell_cex (raw vs basis-corrected, bps) ==")
	for _, k := range keys {
		if k.Direction != "buy_dex_sell_cex" {
			continue
		}
		a := aggs[k]
		if len(a.Ts) == 0 {
			continue
		}
		type hourAgg struct {
			raw, adj []float64
		}
		hours := map[time.Time]*hourAgg{}
		for i, ts := range a.Ts {
			h := ts.UTC().Truncate(time.Hour)
			ha, ok := hours[h]
			if !ok {
				ha = &hourAgg{}
				hours[h] = ha
			}
			ha.raw = append(ha.raw, a.NetRaw[i])
			ha.adj = append(ha.adj, a.Net[i])
		}
		ordered := make([]time.Time, 0, len(hours))
		for h := range hours {
			ordered = append(ordered, h)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Before(ordered[j]) })
		fmt.Fprintf(w, "%s  $%.0f\n", k.Symbol, k.SizeUSD)
		for _, h := range ordered {
			ha := hours[h]
			sort.Float64s(ha.raw)
			sort.Float64s(ha.adj)
			fmt.Fprintf(w, "  %s  n=%-6d raw=%7.1f  adj=%7.1f\n",
				h.Format("2006-01-02 15:04Z"), len(ha.raw),
				percentile(ha.raw, 50), percentile(ha.adj, 50))
		}
	}
	fmt.Fprintln(w, `
A steady raw-vs-adj offset across hours = quote-basis artifact (the stablecoin
conversion, not the market). Hours where adj spikes above zero = candidate
genuine dislocations; verify with the spotcheck tool.`)
}
