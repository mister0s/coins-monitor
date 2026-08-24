// Command report summarizes recorded spread samples: how often, per pair,
// size and direction, the net edge exceeded break-even — plus diagnostics
// separating real edges from measurement artifacts (quote basis, latency).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mister0s/coins-monitor/internal/stats"
)

func main() {
	dataDir := flag.String("data", "data", "directory of recorded .jsonl sample files")
	includeExcluded := flag.Bool("include-excluded", false,
		"include samples that failed the min_pool_tvl_usd or leg-skew gate")
	momentumThreshold := flag.Float64("momentum-threshold", 10,
		"bps of 10s CEX mid change separating flat from strong up/down buckets")
	hourly := flag.Bool("hourly", true, "print the hourly median net edge time series")
	flag.Parse()

	aggs, err := stats.LoadDir(*dataDir, *includeExcluded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	stats.Report(os.Stdout, aggs)
	stats.ReportWindows(os.Stdout, aggs)
	stats.ReportWindowsAdjusted(os.Stdout, aggs)
	stats.ReportMomentum(os.Stdout, aggs, *momentumThreshold)
	if *hourly {
		stats.ReportHourly(os.Stdout, aggs)
	}
}
