// Command report summarizes recorded spread samples: how often, per pair,
// size and direction, the net edge exceeded break-even.
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
		"include samples that failed the min_pool_tvl_usd gate")
	flag.Parse()

	aggs, err := stats.LoadDir(*dataDir, *includeExcluded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	stats.Report(os.Stdout, aggs)
	stats.ReportWindows(os.Stdout, aggs)
}
