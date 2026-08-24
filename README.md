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

Every `poll_interval` (default 2s, per-pair overridable), for each configured
pair and each configured trade size:

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
4. **Record** — append every observation to `data/<PAIR>.jsonl`. Each sample
   carries independent leg timestamps (`cex_ts` = when the top-of-book was
   received, `dex_ts` = when both DEX quotes completed) plus their skew;
   samples are flagged `include_in_stats: false` (with an `exclude_reason`)
   when the pool's estimated TVL is below `min_pool_tvl_usd` **or** the leg
   skew exceeds `max_leg_skew` (default 3s) — recorded either way, so those
   periods stay observable without polluting the profitability statistics.

The point of the 2s cadence is not just "does net edge ever exceed zero" but
**how long profitable windows last** — an edge lasting 900ms is uncapturable
without colocation; the same edge lasting 30s might be tradeable. The report
groups consecutive net-positive samples into windows and reports their
duration distribution (see below).

### RPC request budget

Each sample costs **2 quote requests per trade size** (buy + sell leg), plus
2 TVL requests per pair every `tvl_refresh_interval` (negligible). Per pair:

```
req/s  =  2 × len(trade_sizes_usd) / poll_interval_seconds
```

For the shipped config (3 sizes everywhere, 2s interval, SOL at 10s):

| Endpoint | Pairs | req/s | req/min | ~req/month |
|---|---|---|---|---|
| ethereum RPC | ACX, GLM, FLUX | 9 | 540 | ~23M |
| polygon RPC | POL | 3 | 180 | ~7.8M |
| avalanche RPC | AVAX | 3 | 180 | ~7.8M |
| Jupiter quote API | SOL | 0.6 | 36 | ~1.6M |

Pick an RPC provider tier accordingly (the ethereum endpoint is the heavy
one; free public endpoints generally will not sustain 540 req/min — use a
provider plan sized for ~25M requests/month, or raise the ethereum pairs'
`poll_interval` to 4–6s to halve/third it). `rate_limit_rps` is the hard
cap: one shared limiter per endpoint paces and interleaves all pairs on it,
so a misconfigured interval degrades into queueing (growing leg skew, which
the skew gate then excludes) rather than into RPC bans.

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
samples whose net edge exceeded break-even** — followed by the
**profitable-window analysis**: consecutive net-positive samples grouped into
windows, with window count, duration percentiles (p50/p95/max seconds), and
the edge-weighted mean duration (a long marginal window counts less than a
short fat one). Window durations are upper bounds at the achieved sampling
resolution, which the report prints per series (`res_s`).

`-include-excluded` includes TVL/skew-gated samples in the distributions.

### Diagnostics: is a persistent edge real?

A one-directional edge that never goes away is usually a measurement
artifact, not free money. The report includes three artifact detectors, plus
a manual verification tool — all additive (raw fields are never altered, so
historical data stays comparable):

1. **Quote-basis correction** — when a pair sets `quote_basis_symbol` (e.g.
   `USDCUSDT` for the USDC-quoted LFJ AVAX pool vs the USDT-quoted CEX
   pair), the monitor streams that symbol from the same feed and stores
   basis-corrected edges (`*_adj_bps`) alongside raw ones. The report
   re-runs the window analysis on corrected data; **`surv%`** says how much
   of the raw net-positive time survives. Near 100% → real; near 0% → you
   were measuring the stablecoin basis.
2. **Momentum correlation** — each sample records the CEX mid's change over
   the prior ~10s. The report buckets corrected net edge by momentum
   (down/flat/up, threshold `-momentum-threshold`, default 10 bps). Positive
   edge concentrated in the **up** bucket means the DEX quote lags a rising
   CEX price — latency skew, not capturable opportunity.
3. **Persistence profile** — hourly medians of raw vs corrected net edge
   (`-hourly`). A flat persistent raw-vs-adj offset points at the quote
   basis; isolated spiky hours point at genuine dislocations.
4. **Spot-check** — verify one window end-to-end by hand:

   ```sh
   go run ./cmd/spotcheck -pair AVAX/USDT -at 2026-08-24T12:34:56Z -window 30s
   ```

   dumps every recorded leg (CEX bid/ask, DEX effective prices, basis mid,
   skew, momentum, exclusion flags) around the timestamp, plus the Binance
   1s klines for the same interval.

## Configuration

See the extensively commented [`config.yaml`](config.yaml). Adding a pair is
appending one YAML block. Notes on the shipped pairs:

- **AVAX/USDT (LFJ)** — points at the deep WAVAX/USDC LB v2.2 pool (~$4.5M);
  the native WAVAX/USDt LB pair only holds ~$100k and is left as a commented
  alternative. USDC is treated as $1, same as USDT.
- **SOL/USDT (Jupiter)** — works out of the box via the free
  `lite-api.jup.ag` endpoint; there is no single pool, so no TVL gate.
- **POL/USDT (Uniswap v3)** — WPOL/USDT 0.05% pool on Polygon (~$1M).
- **ACX/USDT, GLM/USDT, FLUX/USDT (Uniswap v3)** — routed USDT→WETH→token;
  the deepest pools are all the 1% fee tier vs WETH. ACX is reasonably deep
  (~$1M); GLM and FLUX v3 pools are very thin (~$55k each — most GLM DEX
  liquidity is in Uniswap v2, which isn't a supported venue), so expect the
  TVL gate to exclude their samples. For routed pairs the TVL estimate
  counts only the base-token side of the final pool (~2x understated).
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
