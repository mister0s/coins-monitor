package dex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mister0s/coins-monitor/internal/evm"
)

// DefaultJupiterQuoteURL is Jupiter's free public quote endpoint.
const DefaultJupiterQuoteURL = "https://lite-api.jup.ag/swap/v1/quote"

// Jupiter quotes Solana swaps through the Jupiter aggregator's HTTP quote
// API. The quote is an aggregate across Raydium, Orca and other venues, which
// is the realistic executable DEX price on Solana.
type Jupiter struct {
	quoteURL      string
	baseMint      string
	quoteMint     string
	baseDecimals  int
	quoteDecimals int
	hc            *http.Client
}

func NewJupiter(quoteURL, baseMint string, baseDecimals int, quoteMint string, quoteDecimals int) (*Jupiter, error) {
	if quoteURL == "" {
		quoteURL = DefaultJupiterQuoteURL
	}
	if baseMint == "" || quoteMint == "" {
		return nil, fmt.Errorf("jupiter: base_mint and quote_mint are required")
	}
	if baseDecimals <= 0 || quoteDecimals <= 0 {
		return nil, fmt.Errorf("jupiter: base_token.decimals and quote_token.decimals are required (mint decimals)")
	}
	return &Jupiter{
		quoteURL:      quoteURL,
		baseMint:      baseMint,
		quoteMint:     quoteMint,
		baseDecimals:  baseDecimals,
		quoteDecimals: quoteDecimals,
		hc:            &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (j *Jupiter) Venue() string { return "jupiter" }

type jupiterQuote struct {
	OutAmount string `json:"outAmount"`
	Error     string `json:"error"`
}

func (j *Jupiter) quote(ctx context.Context, inputMint, outputMint string, amountIn *big.Int) (*big.Int, error) {
	if amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("jupiter: non-positive amountIn")
	}
	q := url.Values{}
	q.Set("inputMint", inputMint)
	q.Set("outputMint", outputMint)
	q.Set("amount", amountIn.String())
	q.Set("swapMode", "ExactIn")
	q.Set("slippageBps", "50")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.quoteURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := j.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out jupiterQuote
	if err := json.Unmarshal(body, &out); err != nil || out.OutAmount == "" {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		if out.Error != "" {
			return nil, fmt.Errorf("jupiter quote error: %s", out.Error)
		}
		return nil, fmt.Errorf("jupiter quote failed (http %d): %s", resp.StatusCode, snippet)
	}
	amountOut, ok := new(big.Int).SetString(out.OutAmount, 10)
	if !ok || amountOut.Sign() <= 0 {
		return nil, fmt.Errorf("jupiter: bad outAmount %q", out.OutAmount)
	}
	return amountOut, nil
}

func (j *Jupiter) QuoteBuyBase(ctx context.Context, quoteIn float64) (float64, error) {
	out, err := j.quote(ctx, j.quoteMint, j.baseMint, evm.ToUnits(quoteIn, j.quoteDecimals))
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, j.baseDecimals), nil
}

func (j *Jupiter) QuoteSellBase(ctx context.Context, baseIn float64) (float64, error) {
	out, err := j.quote(ctx, j.baseMint, j.quoteMint, evm.ToUnits(baseIn, j.baseDecimals))
	if err != nil {
		return 0, err
	}
	return evm.FromUnits(out, j.quoteDecimals), nil
}

// PoolTVLUSD is unsupported: an aggregator quote has no single pool.
func (j *Jupiter) PoolTVLUSD(ctx context.Context, basePriceUSD float64) (float64, error) {
	return 0, ErrTVLUnsupported
}
