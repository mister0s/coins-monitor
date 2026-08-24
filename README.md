# coins-monitor

A CEX–DEX arbitrage **spread monitor** in Go. It empirically measures whether
cross-exchange price gaps between centralized exchanges (Binance, KuCoin) and
DEXes ever exceed a configurable break-even cost — across large-cap and
small-cap coins, at several trade sizes.

**Monitoring only.** This codebase contains no order execution, no private
keys, and no wallet integration of any kind. It reads public market data
(CEX WebSocket/REST endpoints) and performs read-only `eth_call` /
aggregator-quote requests. It cannot trade.

## How it works

Every `poll_interval` (default 15s), for each configured pair and each
configured trade size:

1. **CEX side** — take the live top-of-book (bid/ask) from a Binance
   `bookTicker` WebSocket stream or a KuCoin level-1 REST poll.
2. **DEX side** — fetch two *exact-in, size-aware* quotes:
   - buy base with `size` USD of the quote token → effective **DEX buy price**
   - sell `size / cex_mid` base tokens → effective **DEX sell price**

   Because these are exact-in quotes for the actual size, **pool fees and
   price impact are already embedded** in the effective price.
3. **Spread math** — compute both directions in basis points:
   - `buy_dex_sell_cex`: `(cex_bid / dex_buy_price − 1) × 10⁴`
   - `buy_cex_sell_dex`: `(dex_sell_price / cex_ask − 1) × 10⁴`

   Net edge = gross − `cex_taker_fee_bps` − `gas_usd/size × 10⁴` −
   `extra_buffer_bps`. The model assumes inventory pre-positioned on both
   venues (the standard arb setup); bridge/withdrawal rebalancing costs are
   out of scope. Quote tokens (USDT/USDC) are treated as $1.
4. **Record** — append every observation to `data/<PAIR>.jsonl`. Samples taken
   while the pool's estimated TVL is below `min_pool_tvl_usd` are still
   recorded but flagged `include_in_stats: false` so thin-liquidity periods
   don't pollute the profitability statistics.

### Supported venues

| Side | Venue | Method |
|------|-------|--------|
| CEX | Binance | `bookTicker` WebSocket (combined stream, auto-reconnect) |
| CEX | KuCoin | level-1 orderbook REST polling |
| DEX | Uniswap v3 (Ethereum, Polygon, …) | QuoterV2 `quoteExactInputSingle` / multi-hop `quoteExactInput` via `eth_call` |
| DEX | LFJ / Trader Joe Liquidity Book (Avalanche) | LBPair `getSwapOut` via `eth_call` (walks the bins → real depth) |
| DEX | Velodrome v2 (Optimism) | pool `getAmountOut` via `eth_call` (vAMM/sAMM pools; Slipstream CL not supported) |
| DEX | Jupiter (Solana) | HTTP quote API — aggregate across Raydium, Orca, etc. |

CEX listings are **confirmed at startup** against the venue's REST API
(`exchangeInfo` / level-1 probe); unlisted or non-trading symbols are skipped
with a warning, as is any pair whose DEX probe quote fails (bad pool address,
wrong fee tier, missing route). One broken pair never stops the others.

## Quick start

Requires Go 1.27+ (the `go` directive tracks the latest stable release; with
`GOTOOLCHAIN=auto` — the default — any recent Go installation fetches the
right toolchain automatically).

```sh
# 1. Review config.yaml: fill in the REPLACE_ME pool addresses (see comments)
#    and ideally point `chains:` at your own RPC provider.
# 2. Run the monitor:
go run ./cmd/monitor -config config.yaml

# ... let it collect for hours/days, then summarize:
go run ./cmd/report -data data
```

The report shows, per pair × trade size × direction: sample count, gross and
net spread percentiles (p50/p95/max, in bps), and **`n>0` — the share of
samples whose net edge exceeded break-even**, which is the question this tool
exists to answer.

`-include-excluded` includes TVL-gated samples in the distributions.

## Configuration

See the extensively commented [`config.yaml`](config.yaml). Adding a pair is
appending one YAML block. Notes on the shipped pairs:

- **AVAX/USDT (LFJ)** — fill in the current LB pair address from lfj.gg
  before it activates.
- **SOL/USDT (Jupiter)** — works out of the box via the free
  `lite-api.jup.ag` endpoint; there is no single pool, so no TVL gate.
- **POL/USDT, GLM/USDT, ACX/USDT (Uniswap v3)** — quote via QuoterV2, so a
  pool address is only needed if you want the TVL gate. GLM and ACX route
  through WETH (their USDT liquidity is negligible); verify the final-leg fee
  tier on the Uniswap explorer.
- **FLUX/USDT** — ships disabled: verify the canonical bridged FLUX ERC-20
  address first (FLUX is natively its own chain).
- **VELO/USDT** — ships disabled deliberately: the "VELO" listed on
  Binance/KuCoin is **Velo Labs**, a different asset from Velodrome
  Finance's VELO on Optimism. Enabling it as-is would compare two unrelated
  tokens. Only enable with a CEX that actually lists Velodrome's token.

Interpretation caveats baked into the design: CEX top-of-book is a *touch*
price, not depth-weighted — for the small trade sizes used on thin pairs this
is usually fine, but a large size against a thin CEX book will look better
than reality. Latency between the CEX tick and the DEX quote (one RPC round
trip) means very short-lived dislocations may be over- or under-stated;
treat `n>0` rates as an upper bound on capturable frequency.

## Layout

```
cmd/monitor      long-running sampler
cmd/report       offline summary of recorded samples
internal/config  YAML config, env expansion, validation
internal/cex     Binance WS + KuCoin REST top-of-book feeds
internal/dex     size-aware quoters (Uniswap v3, LFJ, Velodrome, Jupiter)
internal/evm     minimal read-only JSON-RPC + ABI encoding (eth_call only)
internal/engine  sampling loop, break-even math, JSONL recorder
internal/stats   aggregation and report rendering
```

## License

MIT — see [LICENSE](LICENSE).
