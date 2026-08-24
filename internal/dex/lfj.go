package dex

import (
	"context"
	"fmt"
	"math/big"

	"github.com/mister0s/coins-monitor/internal/evm"
)

var (
	selGetSwapOut = evm.Selector("getSwapOut(uint128,bool)")
	selGetTokenX  = evm.Selector("getTokenX()")
	selGetTokenY  = evm.Selector("getTokenY()")
)

// LFJ quotes directly against a Liquidity Book pair (LB v2.1+) using
// getSwapOut, which walks the bins and therefore reflects real depth.
type LFJ struct {
	client        *evm.Client
	pair          string
	baseToken     string
	quoteToken    string
	baseDecimals  int
	quoteDecimals int
	baseIsY       bool // whether base token is the pair's tokenY
}

// NewLFJ verifies the pair holds the configured tokens and learns their X/Y
// orientation from the contract itself.
func NewLFJ(ctx context.Context, client *evm.Client, pair, baseToken string, baseDecimals int, quoteToken string, quoteDecimals int) (*LFJ, error) {
	if _, err := evm.ParseAddress(pair); err != nil {
		return nil, fmt.Errorf("lfj: pool_address must be the LB pair address: %w", err)
	}
	outX, err := client.Call(ctx, pair, selGetTokenX)
	if err != nil {
		return nil, fmt.Errorf("lfj getTokenX: %w", err)
	}
	tokenX, err := evm.ReadAddress(outX, 0)
	if err != nil {
		return nil, err
	}
	outY, err := client.Call(ctx, pair, selGetTokenY)
	if err != nil {
		return nil, fmt.Errorf("lfj getTokenY: %w", err)
	}
	tokenY, err := evm.ReadAddress(outY, 0)
	if err != nil {
		return nil, err
	}
	var baseIsY bool
	switch {
	case evm.EqualAddress(tokenX, baseToken) && evm.EqualAddress(tokenY, quoteToken):
		baseIsY = false
	case evm.EqualAddress(tokenY, baseToken) && evm.EqualAddress(tokenX, quoteToken):
		baseIsY = true
	default:
		return nil, fmt.Errorf("lfj pair %s holds %s/%s, not the configured base/quote tokens", pair, tokenX, tokenY)
	}
	return &LFJ{
		client:        client,
		pair:          pair,
		baseToken:     baseToken,
		quoteToken:    quoteToken,
		baseDecimals:  baseDecimals,
		quoteDecimals: quoteDecimals,
		baseIsY:       baseIsY,
	}, nil
}

func (l *LFJ) Venue() string { return "lfj" }

// getSwapOut(amountIn, swapForY) returns (amountInLeft, amountOut, fee).
// A non-zero amountInLeft means the book cannot absorb the size.
func (l *LFJ) swapOut(ctx context.Context, amountIn *big.Int, swapForY bool) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("lfj: non-positive amountIn")
	}
	if amountIn.BitLen() > 128 {
		return nil, fmt.Errorf("lfj: amountIn exceeds uint128")
	}
	data := append([]byte{}, selGetSwapOut...)
	data = append(data, evm.Word(amountIn)...)
	data = append(data, evm.BoolWord(swapForY)...)
	out, err := l.client.Call(ctx, l.pair, data)
	if err != nil {
		return nil, fmt.Errorf("lfj getSwapOut: %w", err)
	}
	amountInLeft, err := evm.ReadWord(out, 0)
	if err != nil {
		return nil, err
	}
	amountOut, err := evm.ReadWord(out, 1)
	if err != nil {
		return nil, err
	}
	if amountInLeft.Sign() > 0 {
		return nil, fmt.Errorf("lfj: insufficient liquidity (%s of amountIn unfilled)", amountInLeft)
	}
	if amountOut.Sign() <= 0 {
		return nil, fmt.Errorf("lfj: zero amountOut")
	}
	return amountOut, nil
}

func (l *LFJ) QuoteBuyBase(ctx context.Context, quoteIn float64) (float64, error) {
	// Buying base = swapping quote in. swapForY is true when the output is Y,
	// i.e. when base is Y.
	out, err := l.swapOut(ctx, evm.ToUnits(quoteIn, l.quoteDecimals), l.baseIsY)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, l.baseDecimals), nil
}

func (l *LFJ) QuoteSellBase(ctx context.Context, baseIn float64) (float64, error) {
	out, err := l.swapOut(ctx, evm.ToUnits(baseIn, l.baseDecimals), !l.baseIsY)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, l.quoteDecimals), nil
}

func (l *LFJ) PoolTVLUSD(ctx context.Context, basePriceUSD float64) (float64, error) {
	return erc20PoolTVL(ctx, l.client, l.pair, l.baseToken, l.baseDecimals, l.quoteToken, l.quoteDecimals, basePriceUSD)
}
