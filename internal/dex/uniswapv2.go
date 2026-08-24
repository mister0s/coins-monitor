package dex

import (
	"context"
	"fmt"
	"math/big"

	"github.com/mister0s/coins-monitor/internal/evm"
)

// DefaultUniswapV2Router is the canonical UniswapV2Router02 on Ethereum.
const DefaultUniswapV2Router = "0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"

var (
	selGetAmountsOut = evm.Selector("getAmountsOut(uint256,address[])")
	selFactory       = evm.Selector("factory()")
	selGetPair       = evm.Selector("getPair(address,address)")
	selGetReserves   = evm.Selector("getReserves()")
)

// UniswapV2 quotes through the router's getAmountsOut, which applies the
// 0.3% fee and constant-product impact per hop — size-aware by construction.
type UniswapV2 struct {
	client        *evm.Client
	router        string
	pool          string // final pair on the path (base vs quote-or-last-hop)
	baseToken     string
	quoteToken    string
	baseDecimals  int
	quoteDecimals int
	route         []string // intermediate hop tokens, quote->base order
	baseIsToken0  bool
	directPair    bool // pool holds base+quote directly (no route)
}

// NewUniswapV2 wires a v2 quoter and, critically, sanity-checks the pool:
// the configured pool_address must equal the factory-derived pair for the
// final hop's tokens (factory().getPair(...)), which guards against a pasted
// fake/honeypot pool address. An empty pool_address is derived from the
// factory instead.
func NewUniswapV2(ctx context.Context, client *evm.Client, router, pool, baseToken string, baseDecimals int, quoteToken string, quoteDecimals int, route []string) (*UniswapV2, error) {
	if router == "" {
		router = DefaultUniswapV2Router
	}
	if _, err := evm.ParseAddress(baseToken); err != nil {
		return nil, fmt.Errorf("uniswap_v2 base token: %w", err)
	}
	if _, err := evm.ParseAddress(quoteToken); err != nil {
		return nil, fmt.Errorf("uniswap_v2 quote token: %w", err)
	}
	for _, t := range route {
		if _, err := evm.ParseAddress(t); err != nil {
			return nil, fmt.Errorf("uniswap_v2 route hop: %w", err)
		}
	}

	// The final pool pairs the base token with the last token before it on
	// the quote->base path.
	pairedWith := quoteToken
	if len(route) > 0 {
		pairedWith = route[len(route)-1]
	}
	out, err := client.Call(ctx, router, selFactory)
	if err != nil {
		return nil, fmt.Errorf("uniswap_v2 factory(): %w", err)
	}
	factory, err := evm.ReadAddress(out, 0)
	if err != nil {
		return nil, err
	}
	aw, err := evm.AddressWord(baseToken)
	if err != nil {
		return nil, err
	}
	bw, err := evm.AddressWord(pairedWith)
	if err != nil {
		return nil, err
	}
	data := append(append(append([]byte{}, selGetPair...), aw...), bw...)
	out, err = client.Call(ctx, factory, data)
	if err != nil {
		return nil, fmt.Errorf("uniswap_v2 getPair: %w", err)
	}
	derived, err := evm.ReadAddress(out, 0)
	if err != nil {
		return nil, err
	}
	if evm.EqualAddress(derived, "0x0000000000000000000000000000000000000000") {
		return nil, fmt.Errorf("uniswap_v2: factory has no pair for %s/%s", baseToken, pairedWith)
	}
	switch {
	case pool == "":
		pool = derived
	case !evm.EqualAddress(pool, derived):
		return nil, fmt.Errorf("uniswap_v2: configured pool %s does not match factory pair %s for these tokens — possible fake/honeypot pool address", pool, derived)
	}

	out, err = client.Call(ctx, pool, selToken0)
	if err != nil {
		return nil, fmt.Errorf("uniswap_v2 token0: %w", err)
	}
	t0, err := evm.ReadAddress(out, 0)
	if err != nil {
		return nil, err
	}

	return &UniswapV2{
		client:        client,
		router:        router,
		pool:          pool,
		baseToken:     baseToken,
		quoteToken:    quoteToken,
		baseDecimals:  baseDecimals,
		quoteDecimals: quoteDecimals,
		route:         route,
		baseIsToken0:  evm.EqualAddress(t0, baseToken),
		directPair:    len(route) == 0,
	}, nil
}

func (u *UniswapV2) Venue() string { return "uniswap_v2" }

func (u *UniswapV2) path(buyingBase bool) []string {
	p := append([]string{u.quoteToken}, u.route...)
	p = append(p, u.baseToken)
	if !buyingBase {
		for i, j := 0, len(p)-1; i < j; i, j = i+1, j-1 {
			p[i], p[j] = p[j], p[i]
		}
	}
	return p
}

// getAmountsOut ABI-encodes getAmountsOut(uint256, address[]) and returns
// the final element of the returned uint256[].
func (u *UniswapV2) getAmountsOut(ctx context.Context, amountIn *big.Int, path []string) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("uniswap_v2: non-positive amountIn")
	}
	data := append([]byte{}, selGetAmountsOut...)
	data = append(data, evm.Word(amountIn)...)
	data = append(data, evm.Word(big.NewInt(64))...) // offset of address[]
	data = append(data, evm.Word(big.NewInt(int64(len(path))))...)
	for _, t := range path {
		w, err := evm.AddressWord(t)
		if err != nil {
			return nil, err
		}
		data = append(data, w...)
	}
	out, err := u.client.Call(ctx, u.router, data)
	if err != nil {
		return nil, fmt.Errorf("uniswap_v2 getAmountsOut: %w", err)
	}
	// Return layout: [offset][len][amount0..amountN-1]; result is the last.
	off, err := evm.ReadWord(out, 0)
	if err != nil {
		return nil, err
	}
	base := int(off.Int64() / 32)
	n, err := evm.ReadWord(out, base)
	if err != nil {
		return nil, err
	}
	count := int(n.Int64())
	if count != len(path) {
		return nil, fmt.Errorf("uniswap_v2: got %d amounts for %d-token path", count, len(path))
	}
	amountOut, err := evm.ReadWord(out, base+count)
	if err != nil {
		return nil, err
	}
	if amountOut.Sign() <= 0 {
		return nil, fmt.Errorf("uniswap_v2: zero amountOut")
	}
	return amountOut, nil
}

func (u *UniswapV2) QuoteBuyBase(ctx context.Context, quoteIn float64) (float64, error) {
	out, err := u.getAmountsOut(ctx, evm.ToUnits(quoteIn, u.quoteDecimals), u.path(true))
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, u.baseDecimals), nil
}

func (u *UniswapV2) QuoteSellBase(ctx context.Context, baseIn float64) (float64, error) {
	out, err := u.getAmountsOut(ctx, evm.ToUnits(baseIn, u.baseDecimals), u.path(false))
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, u.quoteDecimals), nil
}

// PoolTVLUSD estimates the final pool's TVL from getReserves. For a direct
// base/quote pair both sides are priced; for a routed pair only the base
// reserve is priced (the other side is WETH etc.), understating ~2x — same
// caveat as routed v3 pools.
func (u *UniswapV2) PoolTVLUSD(ctx context.Context, basePriceUSD float64) (float64, error) {
	out, err := u.client.Call(ctx, u.pool, selGetReserves)
	if err != nil {
		return 0, fmt.Errorf("uniswap_v2 getReserves: %w", err)
	}
	r0, err := evm.ReadWord(out, 0)
	if err != nil {
		return 0, err
	}
	r1, err := evm.ReadWord(out, 1)
	if err != nil {
		return 0, err
	}
	baseRes, otherRes := r0, r1
	if !u.baseIsToken0 {
		baseRes, otherRes = r1, r0
	}
	tvl := evm.FromUnits(baseRes, u.baseDecimals) * basePriceUSD
	if u.directPair {
		tvl += evm.FromUnits(otherRes, u.quoteDecimals)
	}
	return tvl, nil
}
