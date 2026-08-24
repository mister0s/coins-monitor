package stats

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Window is a run of consecutive net-positive samples for one
// pair/size/direction. Duration is an UPPER BOUND: the true window could have
// started just after the previous (non-positive) sample and ended just before
// the next one, so each observed run is extended by one sampling gap.
type Window struct {
	Start      time.Time
	End        time.Time // last positive sample's timestamp
	Duration   time.Duration
	Samples    int
	MeanNetBps float64
	MaxNetBps  float64
}

// WindowStats summarizes the profitable windows of one series.
type WindowStats struct {
	Key       Key
	Windows   []Window
	SpacingS  float64 // median sampling gap, seconds
	Count     int
	P50s      float64
	P95s      float64
	MaxS      float64
	EWDurS    float64 // edge-weighted mean duration, seconds
	TotalPosS float64 // total net-positive seconds (upper bound)
}

// FindWindows groups consecutive net-positive samples into windows.
//
// Samples are sorted by timestamp; the series' median inter-sample gap sets
// the resolution. A run breaks when a sample is non-positive or when the gap
// to the next sample exceeds 3x the median spacing (a data gap: monitor
// restart, feed outage) — windows never bridge unobserved time.
func FindWindows(a *Agg) WindowStats {
	ws := WindowStats{Key: a.Key}
	n := len(a.Net)
	if n == 0 {
		return ws
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(x, y int) bool { return a.Ts[idx[x]].Before(a.Ts[idx[y]]) })

	// Median spacing = the sampling resolution actually achieved.
	var gaps []float64
	for k := 1; k < n; k++ {
		gaps = append(gaps, a.Ts[idx[k]].Sub(a.Ts[idx[k-1]]).Seconds())
	}
	spacing := 2.0 // fallback for a single-sample series
	if len(gaps) > 0 {
		sort.Float64s(gaps)
		spacing = gaps[len(gaps)/2]
		if spacing <= 0 {
			spacing = 0.001
		}
	}
	ws.SpacingS = spacing
	maxBridge := time.Duration(3 * spacing * float64(time.Second))
	extension := time.Duration(spacing * float64(time.Second))

	var cur *Window
	var curSum float64
	flush := func() {
		if cur == nil {
			return
		}
		cur.Duration = cur.End.Sub(cur.Start) + extension
		cur.MeanNetBps = curSum / float64(cur.Samples)
		ws.Windows = append(ws.Windows, *cur)
		cur, curSum = nil, 0
	}
	for k := 0; k < n; k++ {
		i := idx[k]
		ts, net := a.Ts[i], a.Net[i]
		if net <= 0 {
			flush()
			continue
		}
		if cur != nil && ts.Sub(cur.End) > maxBridge {
			flush() // data gap: start a new window rather than bridging it
		}
		if cur == nil {
			cur = &Window{Start: ts, End: ts, MaxNetBps: net}
		}
		cur.End = ts
		cur.Samples++
		curSum += net
		if net > cur.MaxNetBps {
			cur.MaxNetBps = net
		}
	}
	flush()

	ws.Count = len(ws.Windows)
	if ws.Count == 0 {
		return ws
	}
	durs := make([]float64, ws.Count)
	var ewNum, ewDen float64
	for i, w := range ws.Windows {
		d := w.Duration.Seconds()
		durs[i] = d
		ws.TotalPosS += d
		ewNum += d * w.MeanNetBps
		ewDen += w.MeanNetBps
	}
	sort.Float64s(durs)
	ws.P50s = percentile(durs, 50)
	ws.P95s = percentile(durs, 95)
	ws.MaxS = percentile(durs, 100)
	if ewDen > 0 {
		ws.EWDurS = ewNum / ewDen
	}
	return ws
}

// ReportWindows renders the profitable-window section of the report.
func ReportWindows(w io.Writer, aggs map[Key]*Agg) {
	keys := sortedKeys(aggs)
	fmt.Fprintln(w, "\n== profitable windows (consecutive samples with net edge > 0) ==")
	fmt.Fprintf(w, "%-10s %-9s %-18s %8s %7s | %7s %7s %7s | %8s %8s\n",
		"pair", "size_usd", "direction", "windows", "res_s",
		"d_p50s", "d_p95s", "d_maxs",
		"ew_dur_s", "pos_s/day")
	fmt.Fprintln(w, strings.Repeat("-", 116))
	currentTier := "\x00"
	for _, k := range keys {
		a := aggs[k]
		if k.Tier != currentTier {
			currentTier = k.Tier
			fmt.Fprintf(w, "== tier: %s ==\n", currentTier)
		}
		ws := FindWindows(a)
		if ws.Count == 0 {
			fmt.Fprintf(w, "%-10s %-9.0f %-18s %8d %7.1f |       -       -       - |        -        -\n",
				k.Symbol, k.SizeUSD, k.Direction, 0, ws.SpacingS)
			continue
		}
		// Normalize total positive time by the observed span for comparability.
		span := a.Ts[len(a.Ts)-1].Sub(a.Ts[0]).Seconds()
		posPerDay := math.NaN()
		if span > 0 {
			posPerDay = ws.TotalPosS / span * 86400
		}
		fmt.Fprintf(w, "%-10s %-9.0f %-18s %8d %7.1f | %7.1f %7.1f %7.1f | %8.1f %8.0f\n",
			k.Symbol, k.SizeUSD, k.Direction, ws.Count, ws.SpacingS,
			ws.P50s, ws.P95s, ws.MaxS, ws.EWDurS, posPerDay)
	}
	fmt.Fprintln(w, `
Window durations are UPPER BOUNDS at the achieved sampling resolution (res_s =
median gap between samples): each observed run is extended by one sampling gap,
and a single positive sample counts as a window of one gap. Runs are never
bridged across data gaps (> 3x median spacing). ew_dur_s weights each window's
duration by its mean net edge — sustained fat windows dominate, long marginal
ones do not: sum(dur x mean_net) / sum(mean_net). pos_s/day = net-positive
seconds per day of observed span (upper bound).`)
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
