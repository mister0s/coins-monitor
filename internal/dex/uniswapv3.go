package dex

import (
	"context"
	"fmt"
	"math/big"

	"github.com/mister0s/coins-monitor/internal/evm"
)

// Canonical QuoterV2 deployment shared across Ethereum, Polygon and Optimism.
const DefaultQuoterV2 = "0x61fFE014bA17989E743c5F6cB21bF9697530B21e"

var (
	selQuoteExactInputSingle = evm.Selector("quoteExactInputSingle((address,address,uint256,uint24,uint160))")
	selQuoteExactInput       = evm.Selector("quoteExactInput(bytes,uint256)")
)

// PathHop is one intermediate hop of a Uniswap v3 route: the token routed
// through, and the fee tier of the pool leading into that token from the
// previous one on the quote->base path.
type PathHop struct {
	Token   string
	FeeTier uint32
}

// UniswapV3 quotes via QuoterV2 through eth_call only. QuoterV2's non-view
// signature is a gas-metering artifact; the call never persists state.
type UniswapV3 struct {
	client        *evm.Client
	quoter        string
	pool          string // optional; enables TVL estimation
	baseToken     string
	quoteToken    string
	baseDecimals  int
	quoteDecimals int
	feeTier       uint32 // fee of the final pool into the base token
	route         []PathHop
}

func NewUniswapV3(client *evm.Client, quoter, pool, baseToken string, baseDecimals int, quoteToken string, quoteDecimals int, feeTier uint32, route []PathHop) (*UniswapV3, error) {
	if quoter == "" {
		quoter = DefaultQuoterV2
	}
	if feeTier == 0 {
		return nil, fmt.Errorf("uniswap_v3: fee_tier is required")
	}
	if _, err := evm.ParseAddress(baseToken); err != nil {
		return nil, fmt.Errorf("uniswap_v3 base token: %w", err)
	}
	if _, err := evm.ParseAddress(quoteToken); err != nil {
		return nil, fmt.Errorf("uniswap_v3 quote token: %w", err)
	}
	for _, h := range route {
		if _, err := evm.ParseAddress(h.Token); err != nil {
			return nil, fmt.Errorf("uniswap_v3 route hop: %w", err)
		}
		if h.FeeTier == 0 {
			return nil, fmt.Errorf("uniswap_v3 route hop %s: fee_tier is required", h.Token)
		}
	}
	return &UniswapV3{
		client:        client,
		quoter:        quoter,
		pool:          pool,
		baseToken:     baseToken,
		quoteToken:    quoteToken,
		baseDecimals:  baseDecimals,
		quoteDecimals: quoteDecimals,
		feeTier:       feeTier,
		route:         route,
	}, nil
}

func (u *UniswapV3) Venue() string { return "uniswap_v3" }

func (u *UniswapV3) QuoteBuyBase(ctx context.Context, quoteIn float64) (float64, error) {
	out, err := u.quoteExactIn(ctx, evm.ToUnits(quoteIn, u.quoteDecimals), true)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, u.baseDecimals), nil
}

func (u *UniswapV3) QuoteSellBase(ctx context.Context, baseIn float64) (float64, error) {
	out, err := u.quoteExactIn(ctx, evm.ToUnits(baseIn, u.baseDecimals), false)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, u.quoteDecimals), nil
}

// quoteExactIn quotes amountIn along the configured path. buyingBase=true
// swaps quote->base; false swaps base->quote (path reversed).
func (u *UniswapV3) quoteExactIn(ctx context.Context, amountIn *big.Int, buyingBase bool) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("uniswap_v3: non-positive amountIn")
	}
	var data []byte
	var err error
	if len(u.route) == 0 {
		tokenIn, tokenOut := u.quoteToken, u.baseToken
		if !buyingBase {
			tokenIn, tokenOut = tokenOut, tokenIn
		}
		data, err = encodeQuoteSingle(tokenIn, tokenOut, amountIn, u.feeTier)
	} else {
		tokens, fees := u.pathTokensAndFees(buyingBase)
		data, err = encodeQuotePath(tokens, fees, amountIn)
	}
	if err != nil {
		return nil, err
	}
	out, err := u.client.Call(ctx, u.quoter, data)
	if err != nil {
		return nil, fmt.Errorf("uniswap_v3 quote: %w", err)
	}
	// Both quoter functions return amountOut as the first head word.
	amountOut, err := evm.ReadWord(out, 0)
	if err != nil {
		return nil, err
	}
	if amountOut.Sign() <= 0 {
		return nil, fmt.Errorf("uniswap_v3: zero amountOut")
	}
	return amountOut, nil
}

// pathTokensAndFees expands the configured route into the alternating
// token/fee sequence Uniswap path encoding expects, in swap direction.
func (u *UniswapV3) pathTokensAndFees(buyingBase bool) ([]string, []uint32) {
	tokens := []string{u.quoteToken}
	fees := []uint32{}
	for _, h := range u.route {
		tokens = append(tokens, h.Token)
		fees = append(fees, h.FeeTier)
	}
	tokens = append(tokens, u.baseToken)
	fees = append(fees, u.feeTier)
	if !buyingBase {
		for i, j := 0, len(tokens)-1; i < j; i, j = i+1, j-1 {
			tokens[i], tokens[j] = tokens[j], tokens[i]
		}
		for i, j := 0, len(fees)-1; i < j; i, j = i+1, j-1 {
			fees[i], fees[j] = fees[j], fees[i]
		}
	}
	return tokens, fees
}

func encodeQuoteSingle(tokenIn, tokenOut string, amountIn *big.Int, fee uint32) ([]byte, error) {
	inW, err := evm.AddressWord(tokenIn)
	if err != nil {
		return nil, err
	}
	outW, err := evm.AddressWord(tokenOut)
	if err != nil {
		return nil, err
	}
	data := append([]byte{}, selQuoteExactInputSingle...)
	data = append(data, inW...)
	data = append(data, outW...)
	data = append(data, evm.Word(amountIn)...)
	data = append(data, evm.Word(big.NewInt(int64(fee)))...)
	data = append(data, evm.Word(big.NewInt(0))...) // sqrtPriceLimitX96 = 0 (no limit)
	return data, nil
}

// encodeQuotePath ABI-encodes quoteExactInput(bytes path, uint256 amountIn),
// where path packs token(20) || fee(3) || token(20) || ... in swap order.
func encodeQuotePath(tokens []string, fees []uint32, amountIn *big.Int) ([]byte, error) {
	var path []byte
	for i, t := range tokens {
		raw, err := evm.ParseAddress(t)
		if err != nil {
			return nil, err
		}
		path = append(path, raw...)
		if i < len(fees) {
			f := fees[i]
			path = append(path, byte(f>>16), byte(f>>8), byte(f))
		}
	}
	data := append([]byte{}, selQuoteExactInput...)
	data = append(data, evm.Word(big.NewInt(64))...) // offset of bytes arg
	data = append(data, evm.Word(amountIn)...)
	data = append(data, evm.Word(big.NewInt(int64(len(path))))...)
	padded := len(path)
	if rem := padded % 32; rem != 0 {
		padded += 32 - rem
	}
	buf := make([]byte, padded)
	copy(buf, path)
	data = append(data, buf...)
	return data, nil
}

// PoolTVLUSD approximates TVL as the pool's raw token balances priced at
// basePriceUSD (base) and $1 (quote). For a v3 pool this includes
// out-of-range liquidity, which is acceptable for a coarse inclusion gate.
func (u *UniswapV3) PoolTVLUSD(ctx context.Context, basePriceUSD float64) (float64, error) {
	if u.pool == "" {
		return 0, ErrTVLUnsupported
	}
	return erc20PoolTVL(ctx, u.client, u.pool, u.baseToken, u.baseDecimals, u.quoteToken, u.quoteDecimals, basePriceUSD)
}
