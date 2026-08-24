// Package dex provides read-only, size-aware swap quotes from decentralized
// exchanges. Every quote is exact-in, so the returned effective price already
// includes the pool fee and the price impact of the requested size.
//
// Quote-token amounts are treated as USD 1:1 (all monitored quote tokens are
// USD stablecoins); a persistent depeg would bias results and is out of scope.
package dex

import (
	"context"
	"errors"
)

// ErrTVLUnsupported is returned by venues that cannot report pool TVL
// (e.g. aggregator quotes with no single pool).
var ErrTVLUnsupported = errors.New("tvl not supported for this venue")

// Quoter quotes both directions of a base/quote market.
type Quoter interface {
	// QuoteBuyBase spends quoteIn units of the quote token (≈ USD) and
	// returns how much base token comes out.
	QuoteBuyBase(ctx context.Context, quoteIn float64) (baseOut float64, err error)
	// QuoteSellBase sells baseIn units of the base token and returns how
	// much quote token (≈ USD) comes out.
	QuoteSellBase(ctx context.Context, baseIn float64) (quoteOut float64, err error)
	// PoolTVLUSD estimates pool TVL using basePriceUSD for the base leg.
	// Returns ErrTVLUnsupported where not applicable.
	PoolTVLUSD(ctx context.Context, basePriceUSD float64) (float64, error)
	Venue() string
}
