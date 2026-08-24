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

	// Quote-basis correction. When the DEX pool is quoted in a different
	// stablecoin than the CEX pair (e.g. LFJ WAVAX/USDC vs Binance
	// AVAX/USDT), BasisSymbol/BasisMid record the live conversion (e.g.
	// USDCUSDT mid = USDT per USDC) and the *_adj_bps fields re-express the
	// DEX leg in CEX-quote terms. With no basis configured, or when the
	// basis book is unavailable (BasisMid 0), adj == raw. Raw fields are
	// never altered, so historical data stays comparable.
	BasisSymbol              string  `json:"basis_symbol,omitempty"`
	BasisMid                 float64 `json:"basis_mid,omitempty"`
	GrossBuyDexSellCexAdjBps float64 `json:"gross_buy_dex_sell_cex_adj_bps"`
	NetBuyDexSellCexAdjBps   float64 `json:"net_buy_dex_sell_cex_adj_bps"`
	GrossBuyCexSellDexAdjBps float64 `json:"gross_buy_cex_sell_dex_adj_bps"`
	NetBuyCexSellDexAdjBps   float64 `json:"net_buy_cex_sell_dex_adj_bps"`

	// CEX mid momentum over the ~10s before this sample, for the
	// trend-correlation diagnostic: positive edges that concentrate in
	// strong-up momentum are latency skew, not opportunity. MomentumOK is
	// false when no sufficiently old mid was available (e.g. right after
	// startup).
	CexMidChg10sBps float64 `json:"cex_mid_chg_10s_bps"`
	MomentumOK      bool    `json:"momentum_ok"`
	// MomentumBucket classifies CexMidChg10sBps against the pair's
	// configured threshold at sample time: "down", "flat", "up" — empty
	// when MomentumOK is false. The threshold used is stored alongside so
	// later re-bucketing is possible.
	MomentumBucket       string  `json:"momentum_bucket,omitempty"`
	MomentumThresholdBps float64 `json:"momentum_threshold_bps,omitempty"`

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

// computeEdges fills the gross/net spread fields, raw and basis-corrected,
// from the recorded prices. DEX prices are in DEX-quote-token units; the
// corrected variants multiply them by BasisMid (CEX-quote per DEX-quote,
// identity when unset) before comparing against the CEX book.
func (s *Sample) computeEdges() {
	costs := costBps(s.CexFeeBps, s.GasUSD, s.BufferBps, s.TradeSizeUSD)
	basis := s.BasisMid
	if basis <= 0 {
		basis = 1
	}
	if s.DexBuyPrice > 0 {
		s.GrossBuyDexSellCexBps = (s.CexBid/s.DexBuyPrice - 1) * 1e4
		s.NetBuyDexSellCexBps = s.GrossBuyDexSellCexBps - costs
		s.GrossBuyDexSellCexAdjBps = (s.CexBid/(s.DexBuyPrice*basis) - 1) * 1e4
		s.NetBuyDexSellCexAdjBps = s.GrossBuyDexSellCexAdjBps - costs
	}
	if s.CexAsk > 0 {
		s.GrossBuyCexSellDexBps = (s.DexSellPrice/s.CexAsk - 1) * 1e4
		s.NetBuyCexSellDexBps = s.GrossBuyCexSellDexBps - costs
		s.GrossBuyCexSellDexAdjBps = (s.DexSellPrice*basis/s.CexAsk - 1) * 1e4
		s.NetBuyCexSellDexAdjBps = s.GrossBuyCexSellDexAdjBps - costs
	}
}
