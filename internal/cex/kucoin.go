package cex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// KucoinFeed polls the public level-1 REST endpoint per symbol. KuCoin's
// WebSocket requires a token handshake; for spread sampling at 15s cadence,
// REST polling is simpler and well within public rate limits.
type KucoinFeed struct {
	restEndpoint string
	symbols      []string // exchange symbols, e.g. VELO-USDT
	interval     time.Duration
	cache        *bookCache
	log          *slog.Logger
	hc           *http.Client
}

func NewKucoinFeed(restEndpoint string, symbols []string, interval time.Duration, log *slog.Logger) *KucoinFeed {
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	return &KucoinFeed{
		restEndpoint: strings.TrimRight(restEndpoint, "/"),
		symbols:      symbols,
		interval:     interval,
		cache:        newBookCache(),
		log:          log.With("venue", "kucoin"),
		hc:           &http.Client{Timeout: 10 * time.Second},
	}
}

func (f *KucoinFeed) Venue() string { return "kucoin" }

func (f *KucoinFeed) Book(symbol string) (Book, bool) {
	return f.cache.get(strings.ToUpper(symbol))
}

type kucoinLevel1 struct {
	Code string `json:"code"`
	Data *struct {
		BestBid string `json:"bestBid"`
		BestAsk string `json:"bestAsk"`
	} `json:"data"`
}

func (f *KucoinFeed) fetch(ctx context.Context, symbol string) (Book, error) {
	u := fmt.Sprintf("%s/api/v1/market/orderbook/level1?symbol=%s", f.restEndpoint, url.QueryEscape(symbol))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Book{}, err
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return Book{}, err
	}
	defer resp.Body.Close()
	var out kucoinLevel1
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Book{}, fmt.Errorf("decode (http %d): %w", resp.StatusCode, err)
	}
	if out.Data == nil {
		return Book{}, fmt.Errorf("no level1 data for %s (code %s) — symbol may not be listed", symbol, out.Code)
	}
	bid, err1 := strconv.ParseFloat(out.Data.BestBid, 64)
	ask, err2 := strconv.ParseFloat(out.Data.BestAsk, 64)
	if err1 != nil || err2 != nil || bid <= 0 || ask <= 0 {
		return Book{}, fmt.Errorf("bad level1 quote for %s: bid=%q ask=%q", symbol, out.Data.BestBid, out.Data.BestAsk)
	}
	return Book{Bid: bid, Ask: ask, Ts: time.Now()}, nil
}

func (f *KucoinFeed) ValidateSymbol(ctx context.Context, symbol string) error {
	_, err := f.fetch(ctx, strings.ToUpper(symbol))
	return err
}

func (f *KucoinFeed) Start(ctx context.Context) {
	for _, s := range f.symbols {
		symbol := strings.ToUpper(s)
		go func() {
			t := time.NewTicker(f.interval)
			defer t.Stop()
			for {
				b, err := f.fetch(ctx, symbol)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					f.log.Warn("level1 poll failed", "symbol", symbol, "err", err)
				} else {
					f.cache.set(symbol, b)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
}
