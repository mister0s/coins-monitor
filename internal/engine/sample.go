package engine

import "time"

// Sample is one observation of the CEX-DEX spread for one pair at one trade
// size, recorded as a JSONL row. All *_bps fields are basis points.
//
// Directions:
//   - buy_dex_sell_cex: buy base on the DEX with quote token, sell at the CEX bid.
//   - buy_cex_sell_dex: buy base at the CEX ask, sell it on the DEX.
//
// DEX prices are effective (size-aware) prices from exact-in quotes, so pool
// fees and price impact are already included. Net edge additionally deducts
// the CEX taker fee, chain gas, and a safety buffer. The model assumes
// inventory is pre-positioned on both venues (no bridge/withdrawal leg).
type Sample struct {
	Ts           time.Time `json:"ts"`
	Symbol       string    `json:"symbol"`
	Tier         string    `json:"tier"`
	CexVenue     string    `json:"cex_venue"`
	DexVenue     string    `json:"dex_venue"`
	Chain        string    `json:"chain"`
	TradeSizeUSD float64   `json:"trade_size_usd"`

	// Leg timestamps: CexTs is when the CEX top-of-book was received, DexTs
	// is when both DEX quotes completed. SkewMs = DexTs - CexTs; a spread
	// computed across a large skew compares prices from different moments,
	// so samples with |skew| > max_leg_skew are excluded from stats.
	CexTs  time.Time `json:"cex_ts"`
	DexTs  time.Time `json:"dex_ts"`
	SkewMs int64     `json:"skew_ms"`

	CexBid float64 `json:"cex_bid"`
	CexAsk float64 `json:"cex_ask"`
	CexMid float64 `json:"cex_mid"`

	// Effective DEX prices for this size (quote token per base token).
	DexBuyPrice  float64 `json:"dex_buy_price"`  // cost per base when buying base on DEX
	DexSellPrice float64 `json:"dex_sell_price"` // proceeds per base when selling base on DEX

	GrossBuyDexSellCexBps float64 `json:"gross_buy_dex_sell_cex_bps"`
	NetBuyDexSellCexBps   float64 `json:"net_buy_dex_sell_cex_bps"`
	GrossBuyCexSellDexBps float64 `json:"gross_buy_cex_sell_dex_bps"`
	NetBuyCexSellDexBps   float64 `json:"net_buy_cex_sell_dex_bps"`

	// Cost inputs used for the net figures.
	CexFeeBps float64 `json:"cex_fee_bps"`
	GasUSD    float64 `json:"gas_usd"`
	BufferBps float64 `json:"buffer_bps"`

	// TVL gate. PoolTVLUSD is -1 when the venue cannot report TVL.
	PoolTVLUSD    float64 `json:"pool_tvl_usd"`
	MinPoolTVLUSD float64 `json:"min_pool_tvl_usd"`
	// IncludeInStats is false when the sample fails a quality gate (TVL or
	// leg skew); it is still recorded so those periods remain observable.
	// ExcludeReason names the gate(s) that failed: "tvl", "skew", "tvl+skew".
	IncludeInStats bool   `json:"include_in_stats"`
	ExcludeReason  string `json:"exclude_reason,omitempty"`
}

// costBps converts the per-trade fixed costs into basis points of the trade size.
func costBps(cexFeeBps, gasUSD, bufferBps, sizeUSD float64) float64 {
	return cexFeeBps + bufferBps + gasUSD/sizeUSD*1e4
}

// computeEdges fills the gross/net spread fields from the raw prices.
func (s *Sample) computeEdges() {
	costs := costBps(s.CexFeeBps, s.GasUSD, s.BufferBps, s.TradeSizeUSD)
	if s.DexBuyPrice > 0 {
		s.GrossBuyDexSellCexBps = (s.CexBid/s.DexBuyPrice - 1) * 1e4
		s.NetBuyDexSellCexBps = s.GrossBuyDexSellCexBps - costs
	}
	if s.CexAsk > 0 {
		s.GrossBuyCexSellDexBps = (s.DexSellPrice/s.CexAsk - 1) * 1e4
		s.NetBuyCexSellDexBps = s.GrossBuyCexSellDexBps - costs
	}
}
