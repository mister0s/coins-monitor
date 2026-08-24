// Package config loads and validates the monitor's YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML values like "15s" parse naturally.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Costs models the break-even cost of one round of arbitrage
// (one CEX taker fill + one DEX swap, inventory pre-positioned on both venues).
// The DEX pool fee and price impact are NOT listed here: they are already
// embedded in the exact-in quotes the DEX connectors return.
type Costs struct {
	CexTakerFeeBps float64            `yaml:"cex_taker_fee_bps"`
	ExtraBufferBps float64            `yaml:"extra_buffer_bps"`
	GasUSD         map[string]float64 `yaml:"gas_usd"` // chain -> estimated swap gas cost in USD
}

// Token identifies an on-chain token. Decimals may be 0, in which case they
// are fetched from the contract at startup (EVM chains only).
type Token struct {
	Address  string `yaml:"address"`
	Decimals int    `yaml:"decimals"`
}

// RouteHop is one intermediate hop of a Uniswap v3 path, e.g. via WETH.
type RouteHop struct {
	Token   string `yaml:"token"`
	FeeTier uint32 `yaml:"fee_tier"` // fee of the pool leading INTO this hop's next leg
}

type CexConfig struct {
	Venue        string `yaml:"venue"`  // binance | kucoin
	Symbol       string `yaml:"symbol"` // optional override; derived from pair symbol otherwise
	WSEndpoint   string `yaml:"ws_endpoint"`
	RestEndpoint string `yaml:"rest_endpoint"`
}

type DexConfig struct {
	Chain       string `yaml:"chain"`
	Venue       string `yaml:"venue"`        // uniswap_v3 | lfj | velodrome | jupiter
	PoolAddress string `yaml:"pool_address"` // pool/pair contract (TVL checks; quoting for lfj/velodrome)

	// uniswap_v3
	QuoterAddress string     `yaml:"quoter_address"` // QuoterV2; defaulted per chain if empty
	FeeTier       uint32     `yaml:"fee_tier"`
	Route         []RouteHop `yaml:"route"` // optional intermediate hops (multi-hop path)

	// EVM venues
	BaseToken  Token `yaml:"base_token"`
	QuoteToken Token `yaml:"quote_token"`

	// jupiter (Solana)
	QuoteURL  string `yaml:"quote_url"` // defaults to the public lite endpoint
	BaseMint  string `yaml:"base_mint"`
	QuoteMint string `yaml:"quote_mint"`

	// QuoteBasisSymbol names a CEX symbol (e.g. USDCUSDT) whose mid converts
	// this DEX's quote-token units into the CEX pair's quote units. Set it
	// when the DEX pool is quoted in a different stablecoin than the CEX
	// pair, so the basis (e.g. a USDC/USDT depeg of a few bps) is measured
	// per sample instead of assumed to be exactly 1. Samples store both the
	// raw and the basis-corrected edge.
	QuoteBasisSymbol string `yaml:"quote_basis_symbol"`
}

type Pair struct {
	Symbol        string    `yaml:"symbol"` // e.g. AVAX/USDT
	Tier          string    `yaml:"tier"`   // large | mid | small
	Enabled       *bool     `yaml:"enabled"`
	CEX           CexConfig `yaml:"cex"`
	DEX           DexConfig `yaml:"dex"`
	TradeSizesUSD []float64 `yaml:"trade_sizes_usd"`
	MinPoolTVLUSD float64   `yaml:"min_pool_tvl_usd"`
	// PollInterval overrides the global poll_interval for this pair (e.g. to
	// slow down a pair on a rate-limited endpoint).
	PollInterval Duration `yaml:"poll_interval"`
}

// EffectiveInterval returns this pair's sampling interval.
func (p Pair) EffectiveInterval(global Duration) Duration {
	if p.PollInterval > 0 {
		return p.PollInterval
	}
	return global
}

func (p Pair) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// BaseSymbol returns e.g. "AVAX" for "AVAX/USDT".
func (p Pair) BaseSymbol() string {
	if i := strings.Index(p.Symbol, "/"); i > 0 {
		return p.Symbol[:i]
	}
	return p.Symbol
}

type Config struct {
	PollInterval       Duration `yaml:"poll_interval"`
	TVLRefreshInterval Duration `yaml:"tvl_refresh_interval"`
	SummaryInterval    Duration `yaml:"summary_interval"`
	MaxBookAge         Duration `yaml:"max_book_age"` // reject CEX quotes older than this
	// MaxLegSkew is the maximum |dex_ts - cex_ts| for a sample to count in
	// profitability stats; larger-skew samples are recorded but excluded.
	MaxLegSkew Duration          `yaml:"max_leg_skew"`
	DataDir    string            `yaml:"data_dir"`
	Chains     map[string]string `yaml:"chains"` // chain -> JSON-RPC URL (supports ${ENV} expansion)
	// RateLimitRPS caps outbound quote/TVL requests per endpoint, keyed by
	// chain name ("solana" covers the Jupiter quote API). One shared limiter
	// per endpoint paces and staggers all pairs using it. 0/absent = default.
	RateLimitRPS map[string]float64 `yaml:"rate_limit_rps"`
	Costs        Costs              `yaml:"costs"`
	Pairs        []Pair             `yaml:"pairs"`
}

// DefaultRateLimitRPS applies when rate_limit_rps has no entry for a chain.
const DefaultRateLimitRPS = 10.0

// RateLimitFor returns the configured RPS cap for an endpoint key.
func (c *Config) RateLimitFor(chain string) float64 {
	if v, ok := c.RateLimitRPS[chain]; ok {
		return v
	}
	return DefaultRateLimitRPS
}

// Load reads, env-expands, parses and validates the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := os.Expand(string(raw), func(key string) string {
		return os.Getenv(key)
	})
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.PollInterval == 0 {
		c.PollInterval = Duration(2 * time.Second)
	}
	if c.MaxLegSkew == 0 {
		c.MaxLegSkew = Duration(3 * time.Second)
	}
	if c.TVLRefreshInterval == 0 {
		c.TVLRefreshInterval = Duration(10 * time.Minute)
	}
	if c.SummaryInterval == 0 {
		c.SummaryInterval = Duration(60 * time.Second)
	}
	if c.MaxBookAge == 0 {
		c.MaxBookAge = Duration(10 * time.Second)
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	for i := range c.Pairs {
		p := &c.Pairs[i]
		switch p.CEX.Venue {
		case "binance":
			if p.CEX.WSEndpoint == "" {
				p.CEX.WSEndpoint = "wss://stream.binance.com:9443"
			}
			if p.CEX.RestEndpoint == "" {
				p.CEX.RestEndpoint = "https://api.binance.com"
			}
			if p.CEX.Symbol == "" {
				p.CEX.Symbol = strings.ToUpper(strings.ReplaceAll(p.Symbol, "/", ""))
			}
		case "kucoin":
			if p.CEX.RestEndpoint == "" {
				p.CEX.RestEndpoint = "https://api.kucoin.com"
			}
			if p.CEX.Symbol == "" {
				p.CEX.Symbol = strings.ToUpper(strings.ReplaceAll(p.Symbol, "/", "-"))
			}
		}
	}
}

func (c *Config) validate() error {
	if len(c.Pairs) == 0 {
		return fmt.Errorf("config: no pairs defined")
	}
	seen := map[string]bool{}
	for _, p := range c.Pairs {
		if p.Symbol == "" {
			return fmt.Errorf("config: pair with empty symbol")
		}
		if seen[p.Symbol] {
			return fmt.Errorf("config: duplicate pair %s", p.Symbol)
		}
		seen[p.Symbol] = true
		if !p.IsEnabled() {
			continue
		}
		if len(p.TradeSizesUSD) == 0 {
			return fmt.Errorf("config: %s has no trade_sizes_usd", p.Symbol)
		}
		for _, s := range p.TradeSizesUSD {
			if s <= 0 {
				return fmt.Errorf("config: %s has non-positive trade size %v", p.Symbol, s)
			}
		}
		switch p.CEX.Venue {
		case "binance", "kucoin":
		default:
			return fmt.Errorf("config: %s: unsupported cex venue %q", p.Symbol, p.CEX.Venue)
		}
		switch p.DEX.Venue {
		case "uniswap_v3", "lfj", "velodrome", "jupiter":
		default:
			return fmt.Errorf("config: %s: unsupported dex venue %q", p.Symbol, p.DEX.Venue)
		}
	}
	return nil
}
