package dex

import (
	"context"
	"fmt"
	"math/big"

	"github.com/mister0s/coins-monitor/internal/evm"
)

var (
	selGetAmountOut = evm.Selector("getAmountOut(uint256,address)")
	selToken0       = evm.Selector("token0()")
	selToken1       = evm.Selector("token1()")
)

// Velodrome quotes directly against a v2 (vAMM/sAMM) pool via getAmountOut,
// which applies the pool's fee and curve. Slipstream (CL) pools do not expose
// this method and are not supported.
type Velodrome struct {
	client        *evm.Client
	pool          string
	baseToken     string
	quoteToken    string
	baseDecimals  int
	quoteDecimals int
}

// NewVelodrome verifies the pool holds the configured token pair.
func NewVelodrome(ctx context.Context, client *evm.Client, pool, baseToken string, baseDecimals int, quoteToken string, quoteDecimals int) (*Velodrome, error) {
	if _, err := evm.ParseAddress(pool); err != nil {
		return nil, fmt.Errorf("velodrome: pool_address is required: %w", err)
	}
	out0, err := client.Call(ctx, pool, selToken0)
	if err != nil {
		return nil, fmt.Errorf("velodrome token0: %w", err)
	}
	t0, err := evm.ReadAddress(out0, 0)
	if err != nil {
		return nil, err
	}
	out1, err := client.Call(ctx, pool, selToken1)
	if err != nil {
		return nil, fmt.Errorf("velodrome token1: %w", err)
	}
	t1, err := evm.ReadAddress(out1, 0)
	if err != nil {
		return nil, err
	}
	ok := (evm.EqualAddress(t0, baseToken) && evm.EqualAddress(t1, quoteToken)) ||
		(evm.EqualAddress(t1, baseToken) && evm.EqualAddress(t0, quoteToken))
	if !ok {
		return nil, fmt.Errorf("velodrome pool %s holds %s/%s, not the configured base/quote tokens", pool, t0, t1)
	}
	return &Velodrome{
		client:        client,
		pool:          pool,
		baseToken:     baseToken,
		quoteToken:    quoteToken,
		baseDecimals:  baseDecimals,
		quoteDecimals: quoteDecimals,
	}, nil
}

func (v *Velodrome) Venue() string { return "velodrome" }

func (v *Velodrome) Source() string { return v.client.URL() }

func (v *Velodrome) getAmountOut(ctx context.Context, amountIn *big.Int, tokenIn string) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("velodrome: non-positive amountIn")
	}
	tw, err := evm.AddressWord(tokenIn)
	if err != nil {
		return nil, err
	}
	data := append([]byte{}, selGetAmountOut...)
	data = append(data, evm.Word(amountIn)...)
	data = append(data, tw...)
	out, err := v.client.Call(ctx, v.pool, data)
	if err != nil {
		return nil, fmt.Errorf("velodrome getAmountOut: %w", err)
	}
	amountOut, err := evm.ReadWord(out, 0)
	if err != nil {
		return nil, err
	}
	if amountOut.Sign() <= 0 {
		return nil, fmt.Errorf("velodrome: zero amountOut")
	}
	return amountOut, nil
}

func (v *Velodrome) QuoteBuyBase(ctx context.Context, quoteIn float64) (float64, error) {
	out, err := v.getAmountOut(ctx, evm.ToUnits(quoteIn, v.quoteDecimals), v.quoteToken)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, v.baseDecimals), nil
}

func (v *Velodrome) QuoteSellBase(ctx context.Context, baseIn float64) (float64, error) {
	out, err := v.getAmountOut(ctx, evm.ToUnits(baseIn, v.baseDecimals), v.baseToken)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, v.quoteDecimals), nil
}

func (v *Velodrome) PoolTVLUSD(ctx context.Context, basePriceUSD float64) (float64, error) {
	return erc20PoolTVL(ctx, v.client, v.pool, v.baseToken, v.baseDecimals, v.quoteToken, v.quoteDecimals, basePriceUSD)
}
