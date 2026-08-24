package dex

import (
	"context"

	"github.com/mister0s/coins-monitor/internal/evm"
)

// erc20PoolTVL sums a pool contract's base and quote token balances, priced
// at basePriceUSD and $1 respectively.
func erc20PoolTVL(ctx context.Context, client *evm.Client, pool, baseToken string, baseDecimals int, quoteToken string, quoteDecimals int, basePriceUSD float64) (float64, error) {
	baseBal, err := client.ERC20BalanceOf(ctx, baseToken, pool)
	if err != nil {
		return 0, err
	}
	quoteBal, err := client.ERC20BalanceOf(ctx, quoteToken, pool)
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(baseBal, baseDecimals)*basePriceUSD + evm.FromUnits(quoteBal, quoteDecimals), nil
}
