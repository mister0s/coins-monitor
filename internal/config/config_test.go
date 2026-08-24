package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The shipped config.yaml must always parse and validate.
func TestLoadShippedConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatalf("load shipped config: %v", err)
	}
	if len(cfg.Pairs) != 7 {
		t.Errorf("pairs = %d, want 7", len(cfg.Pairs))
	}
	byName := map[string]Pair{}
	for _, p := range cfg.Pairs {
		byName[p.Symbol] = p
	}
	if p := byName["VELO/USDT"]; p.IsEnabled() {
		t.Error("VELO/USDT must ship disabled (ticker collision with Velo Labs)")
	}
	if p := byName["AVAX/USDT"]; p.CEX.Symbol != "AVAXUSDT" {
		t.Errorf("derived binance symbol = %q", p.CEX.Symbol)
	}
	if p := byName["POL/USDT"]; p.CEX.Symbol != "POLUSDT" {
		t.Errorf("POL cex symbol override = %q", p.CEX.Symbol)
	}
	if p := byName["GLM/USDT"]; len(p.DEX.Route) != 1 || p.DEX.Route[0].FeeTier != 500 {
		t.Errorf("GLM route = %+v", p.DEX.Route)
	}
	if cfg.PollInterval.Std() != 15*time.Second {
		t.Errorf("poll interval = %v", cfg.PollInterval.Std())
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("TEST_RPC_URL", "https://rpc.example.test")
	path := writeTemp(t, `
chains: { ethereum: "${TEST_RPC_URL}" }
pairs:
  - symbol: GLM/USDT
    tier: small
    cex: { venue: binance }
    dex:
      chain: ethereum
      venue: uniswap_v3
      fee_tier: 3000
      base_token: { address: "0x7DD9c5Cba05E151C895FDe1CF355C9A1D5DA6429" }
      quote_token: { address: "0xdAC17F958D2ee523a2206206994597C13D831ec7" }
    trade_sizes_usd: [100]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Chains["ethereum"] != "https://rpc.example.test" {
		t.Errorf("env expansion failed: %q", cfg.Chains["ethereum"])
	}
	if cfg.DataDir != "data" || cfg.MaxBookAge.Std() != 10*time.Second {
		t.Error("defaults not applied")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"no pairs": `pairs: []`,
		"bad venue": `
pairs:
  - symbol: A/B
    cex: { venue: coinbase }
    dex: { chain: ethereum, venue: uniswap_v3 }
    trade_sizes_usd: [100]`,
		"bad dex venue": `
pairs:
  - symbol: A/B
    cex: { venue: binance }
    dex: { chain: ethereum, venue: sushiswap }
    trade_sizes_usd: [100]`,
		"no sizes": `
pairs:
  - symbol: A/B
    cex: { venue: binance }
    dex: { chain: ethereum, venue: uniswap_v3 }`,
		"duplicate": `
pairs:
  - symbol: A/B
    cex: { venue: binance }
    dex: { chain: ethereum, venue: uniswap_v3 }
    trade_sizes_usd: [100]
  - symbol: A/B
    cex: { venue: binance }
    dex: { chain: ethereum, venue: uniswap_v3 }
    trade_sizes_usd: [100]`,
		"unknown field": `
pairz: []`,
	}
	for name, content := range cases {
		if _, err := Load(writeTemp(t, content)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestDisabledPairSkipsValidation(t *testing.T) {
	path := writeTemp(t, `
pairs:
  - symbol: A/B
    enabled: false
    cex: { venue: binance }
    dex: { chain: ethereum, venue: uniswap_v3 }
`)
	if _, err := Load(path); err != nil {
		t.Errorf("disabled pair should not need trade sizes: %v", err)
	}
}
