// Package stats aggregates recorded samples into per-pair/size/direction
// spread distributions to answer the core question: how often does the gross
// gap exceed break-even cost?
package stats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
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
	Key      Key
	Ts       []time.Time // parallel to Gross/Net; not necessarily sorted
	Gross    []float64
	Net      []float64
	Excluded int // samples dropped by a quality gate (TVL / leg skew)
}

func (a *Agg) add(ts time.Time, gross, net float64) {
	a.Ts = append(a.Ts, ts)
	a.Gross = append(a.Gross, gross)
	a.Net = append(a.Net, net)
}

// LoadDir reads every *.jsonl file under dir and aggregates the samples.
// Samples flagged include_in_stats=false are counted but excluded from the
// distributions unless includeExcluded is true.
func LoadDir(dir string, includeExcluded bool) (map[Key]*Agg, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .jsonl files found in %s", dir)
	}
	aggs := map[Key]*Agg{}
	for _, f := range files {
		if err := loadFile(f, includeExcluded, aggs); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return aggs, nil
}

func loadFile(path string, includeExcluded bool, aggs map[Key]*Agg) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 1 {
			var s engine.Sample
			if jerr := json.Unmarshal(line, &s); jerr == nil && s.Symbol != "" {
				record(aggs, s, includeExcluded)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func record(aggs map[Key]*Agg, s engine.Sample, includeExcluded bool) {
	for _, d := range []struct {
		name       string
		gross, net float64
	}{
		{"buy_dex_sell_cex", s.GrossBuyDexSellCexBps, s.NetBuyDexSellCexBps},
		{"buy_cex_sell_dex", s.GrossBuyCexSellDexBps, s.NetBuyCexSellDexBps},
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
		a.add(s.Ts, d.gross, d.net)
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

// Report renders a plain-text summary table grouped by tier.
func Report(w io.Writer, aggs map[Key]*Agg) {
	keys := sortedKeys(aggs)
	currentTier := "\x00"
	fmt.Fprintf(w, "%-10s %-9s %-18s %6s | %8s %8s %8s | %8s %8s %8s | %6s %8s\n",
		"pair", "size_usd", "direction", "n",
		"g_p50", "g_p95", "g_max",
		"n_p50", "n_p95", "n_max",
		"n>0", "excluded")
	fmt.Fprintln(w, strings.Repeat("-", 132))
	for _, k := range keys {
		a := aggs[k]
		if k.Tier != currentTier {
			currentTier = k.Tier
			fmt.Fprintf(w, "== tier: %s ==\n", currentTier)
		}
		g := append([]float64(nil), a.Gross...)
		n := append([]float64(nil), a.Net...)
		sort.Float64s(g)
		sort.Float64s(n)
		pos := 0
		for _, v := range n {
			if v > 0 {
				pos++
			}
		}
		posPct := math.NaN()
		if len(n) > 0 {
			posPct = float64(pos) / float64(len(n)) * 100
		}
		fmt.Fprintf(w, "%-10s %-9.0f %-18s %6d | %8.1f %8.1f %8.1f | %8.1f %8.1f %8.1f | %5.1f%% %8d\n",
			k.Symbol, k.SizeUSD, k.Direction, len(n),
			percentile(g, 50), percentile(g, 95), percentile(g, 100),
			percentile(n, 50), percentile(n, 95), percentile(n, 100),
			posPct, a.Excluded)
	}
	fmt.Fprintln(w, "\nAll spread columns are basis points. g_* = gross spread (DEX fees and price")
	fmt.Fprintln(w, "impact already included), n_* = net of CEX taker fee + gas + buffer.")
	fmt.Fprintln(w, "n>0 = share of samples whose net edge exceeded break-even.")
}
