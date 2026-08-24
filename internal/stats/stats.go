// Package stats aggregates recorded samples into per-pair/size/direction
// spread distributions to answer the core question: how often does the gap
// exceed break-even cost?
//
// The CANONICAL metric throughout is the quote-basis-corrected edge (the DEX
// leg re-expressed in CEX quote units). The uncorrected ("raw") edge is kept
// as a debug series only: it is what a naive USDC==USDT comparison would
// have measured.
package stats

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mister0s/coins-monitor/internal/engine"
)

type Key struct {
	Tier      string
	Symbol    string
	SizeUSD   float64
	Direction string // buy_dex_sell_cex | buy_cex_sell_dex
}

type Agg struct {
	Key Key
	Ts  []time.Time // parallel to the value slices; not necessarily sorted
	// Gross/Net are the CANONICAL basis-corrected series. NetRaw is the
	// uncorrected debug series; it equals Net when no basis is configured
	// (identity correction) and for legacy rows predating the correction.
	Gross  []float64
	Net    []float64
	NetRaw []float64
	// Momentum is the CEX mid change over the prior ~10s in bps; NaN when
	// the sample had no valid momentum reference. Bucket is the stored
	// per-pair-threshold classification ("down"/"flat"/"up", "" legacy).
	Momentum []float64
	Bucket   []string
	Excluded int // samples dropped by a quality gate (TVL / leg skew)
}

func (a *Agg) add(ts time.Time, gross, net, netRaw, momentum float64, bucket string) {
	a.Ts = append(a.Ts, ts)
	a.Gross = append(a.Gross, gross)
	a.Net = append(a.Net, net)
	a.NetRaw = append(a.NetRaw, netRaw)
	a.Momentum = append(a.Momentum, momentum)
	a.Bucket = append(a.Bucket, bucket)
}

// Record folds one sample into the aggregation. Samples flagged
// include_in_stats=false are counted but excluded unless includeExcluded.
func Record(aggs map[Key]*Agg, s engine.Sample, includeExcluded bool) {
	record(aggs, s, includeExcluded)
}

func record(aggs map[Key]*Agg, s engine.Sample, includeExcluded bool) {
	// Rows written before the basis correction existed have zero in every
	// adj field; fall back to raw for those so old data stays usable.
	legacy := s.GrossBuyDexSellCexAdjBps == 0 && s.NetBuyDexSellCexAdjBps == 0 &&
		s.GrossBuyCexSellDexAdjBps == 0 && s.NetBuyCexSellDexAdjBps == 0 &&
		(s.NetBuyDexSellCexBps != 0 || s.NetBuyCexSellDexBps != 0)
	momentum := math.NaN()
	if s.MomentumOK {
		momentum = s.CexMidChg10sBps
	}
	for _, d := range []struct {
		name                               string
		grossAdj, netAdj, grossRaw, netRaw float64
	}{
		{"buy_dex_sell_cex", s.GrossBuyDexSellCexAdjBps, s.NetBuyDexSellCexAdjBps,
			s.GrossBuyDexSellCexBps, s.NetBuyDexSellCexBps},
		{"buy_cex_sell_dex", s.GrossBuyCexSellDexAdjBps, s.NetBuyCexSellDexAdjBps,
			s.GrossBuyCexSellDexBps, s.NetBuyCexSellDexBps},
	} {
		k := Key{Tier: s.Tier, Symbol: s.Symbol, SizeUSD: s.TradeSizeUSD, Direction: d.name}
		a, ok := aggs[k]
		if !ok {
			a = &Agg{Key: k}
			aggs[k] = a
		}
		if !s.IncludeInStats && !includeExcluded {
			a.Excluded++
			continue
		}
		gross, net := d.grossAdj, d.netAdj
		if legacy {
			gross, net = d.grossRaw, d.netRaw
		}
		a.add(s.Ts, gross, net, d.netRaw, momentum, s.MomentumBucket)
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	idx := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// Report renders the main summary table, grouped by tier. All spread columns
// are the basis-corrected edge; raw_p50 is the uncorrected debug median.
func Report(w io.Writer, aggs map[Key]*Agg) {
	keys := sortedKeys(aggs)
	currentTier := "\x00"
	fmt.Fprintf(w, "%-10s %-9s %-18s %6s | %8s %8s %8s | %8s %8s %8s | %6s %8s %8s\n",
		"pair", "size_usd", "direction", "n",
		"g_p50", "g_p95", "g_max",
		"n_p50", "n_p95", "n_max",
		"n>0", "raw_p50", "excluded")
	fmt.Fprintln(w, strings.Repeat("-", 141))
	for _, k := range keys {
		a := aggs[k]
		if k.Tier != currentTier {
			currentTier = k.Tier
			fmt.Fprintf(w, "== tier: %s ==\n", currentTier)
		}
		g := append([]float64(nil), a.Gross...)
		n := append([]float64(nil), a.Net...)
		nr := append([]float64(nil), a.NetRaw...)
		sort.Float64s(g)
		sort.Float64s(n)
		sort.Float64s(nr)
		pos := 0
		for _, v := range a.Net {
			if v > 0 {
				pos++
			}
		}
		posPct := math.NaN()
		if len(n) > 0 {
			posPct = float64(pos) / float64(len(n)) * 100
		}
		fmt.Fprintf(w, "%-10s %-9.0f %-18s %6d | %8.1f %8.1f %8.1f | %8.1f %8.1f %8.1f | %5.1f%% %8.1f %8d\n",
			k.Symbol, k.SizeUSD, k.Direction, len(n),
			percentile(g, 50), percentile(g, 95), percentile(g, 100),
			percentile(n, 50), percentile(n, 95), percentile(n, 100),
			posPct, percentile(nr, 50), a.Excluded)
	}
	fmt.Fprintln(w, "\nAll spread columns are basis points of the BASIS-CORRECTED edge (DEX leg in")
	fmt.Fprintln(w, "CEX quote units; DEX fees and price impact already included). n_* = net of")
	fmt.Fprintln(w, "CEX taker fee + gas + buffer; n>0 = share of samples above break-even.")
	fmt.Fprintln(w, "raw_p50 = uncorrected net median (debug; what a USDC==USDT assumption shows).")
}

// sortedKeys orders group keys tier -> symbol -> size -> direction.
func sortedKeys(aggs map[Key]*Agg) []Key {
	keys := make([]Key, 0, len(aggs))
	for k := range aggs {
		keys = append(keys, k)
	}
	tierRank := map[string]int{"large": 0, "mid": 1, "small": 2}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if tierRank[a.Tier] != tierRank[b.Tier] {
			return tierRank[a.Tier] < tierRank[b.Tier]
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.SizeUSD != b.SizeUSD {
			return a.SizeUSD < b.SizeUSD
		}
		return a.Direction < b.Direction
	})
	return keys
}
