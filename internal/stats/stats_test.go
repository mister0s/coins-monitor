package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mister0s/coins-monitor/internal/engine"
)

func TestPercentile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	if p := percentile(v, 50); p != 3 {
		t.Errorf("p50 = %v", p)
	}
	if p := percentile(v, 100); p != 5 {
		t.Errorf("p100 = %v", p)
	}
	if p := percentile(v, 0); p != 1 {
		t.Errorf("p0 = %v", p)
	}
}

func TestLoadDirAndReport(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "GLM-USDT.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i, inc := range []bool{true, true, false} {
		s := engine.Sample{
			Ts: time.Now(), Symbol: "GLM/USDT", Tier: "small", TradeSizeUSD: 100,
			GrossBuyDexSellCexBps: float64(10 + i), NetBuyDexSellCexBps: float64(i - 1),
			GrossBuyCexSellDexBps: -5, NetBuyCexSellDexBps: -25,
			IncludeInStats: inc,
		}
		if err := enc.Encode(s); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	aggs, err := LoadDir(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	k := Key{Tier: "small", Symbol: "GLM/USDT", SizeUSD: 100, Direction: "buy_dex_sell_cex"}
	a := aggs[k]
	if a == nil {
		t.Fatal("missing agg")
	}
	if len(a.Net) != 2 || a.Excluded != 1 {
		t.Errorf("n=%d excluded=%d, want 2/1", len(a.Net), a.Excluded)
	}

	var sb strings.Builder
	Report(&sb, aggs)
	out := sb.String()
	if !strings.Contains(out, "GLM/USDT") || !strings.Contains(out, "tier: small") {
		t.Errorf("report missing expected content:\n%s", out)
	}

	// includeExcluded pulls the gated sample back in.
	aggs, err = LoadDir(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if a := aggs[k]; len(a.Net) != 3 {
		t.Errorf("with excluded: n=%d, want 3", len(a.Net))
	}
}
