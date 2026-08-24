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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// SnapshotBinance fetches one live top-of-book over REST — used by the
// startup self-test as a transport independent of the websocket.
func SnapshotBinance(ctx context.Context, restEndpoint, symbol string) (Book, error) {
	u := fmt.Sprintf("%s/api/v3/ticker/bookTicker?symbol=%s",
		strings.TrimRight(restEndpoint, "/"), url.QueryEscape(strings.ToUpper(symbol)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Book{}, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return Book{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Book{}, fmt.Errorf("binance bookTicker HTTP %d", resp.StatusCode)
	}
	var out struct {
		Bid string `json:"bidPrice"`
		Ask string `json:"askPrice"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Book{}, err
	}
	bid, err1 := strconv.ParseFloat(out.Bid, 64)
	ask, err2 := strconv.ParseFloat(out.Ask, 64)
	if err1 != nil || err2 != nil || bid <= 0 || ask <= 0 {
		return Book{}, fmt.Errorf("bad bookTicker for %s: bid=%q ask=%q", symbol, out.Bid, out.Ask)
	}
	return Book{Bid: bid, Ask: ask, Ts: time.Now(), Source: u, ConnID: "binance-rest"}, nil
}

// BinanceFeed streams bookTicker updates for a set of symbols over one
// combined-stream WebSocket connection, reconnecting with backoff.
type BinanceFeed struct {
	wsEndpoint   string
	restEndpoint string
	symbols      []string // exchange symbols, e.g. AVAXUSDT
	cache        *bookCache
	log          *slog.Logger
	hc           *http.Client
	connSeq      atomic.Int64 // increments per (re)connect, for provenance
}

func NewBinanceFeed(wsEndpoint, restEndpoint string, symbols []string, log *slog.Logger) *BinanceFeed {
	return &BinanceFeed{
		wsEndpoint:   strings.TrimRight(wsEndpoint, "/"),
		restEndpoint: strings.TrimRight(restEndpoint, "/"),
		symbols:      symbols,
		cache:        newBookCache(),
		log:          log.With("venue", "binance"),
		hc:           &http.Client{Timeout: 10 * time.Second},
	}
}

func (f *BinanceFeed) Venue() string { return "binance" }

func (f *BinanceFeed) Book(symbol string) (Book, bool) {
	return f.cache.get(strings.ToUpper(symbol))
}

// ValidateSymbol checks the symbol is listed and TRADING via exchangeInfo.
func (f *BinanceFeed) ValidateSymbol(ctx context.Context, symbol string) error {
	u := fmt.Sprintf("%s/api/v3/exchangeInfo?symbol=%s", f.restEndpoint, url.QueryEscape(strings.ToUpper(symbol)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("symbol %s is not listed on binance", symbol)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("binance exchangeInfo returned HTTP %d", resp.StatusCode)
	}
	var info struct {
		Symbols []struct {
			Symbol string `json:"symbol"`
			Status string `json:"status"`
		} `json:"symbols"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return err
	}
	for _, s := range info.Symbols {
		if s.Symbol == strings.ToUpper(symbol) {
			if s.Status != "TRADING" {
				return fmt.Errorf("symbol %s on binance has status %s", symbol, s.Status)
			}
			return nil
		}
	}
	return fmt.Errorf("symbol %s is not listed on binance", symbol)
}

func (f *BinanceFeed) streamURL() string {
	streams := make([]string, len(f.symbols))
	for i, s := range f.symbols {
		streams[i] = strings.ToLower(s) + "@bookTicker"
	}
	return f.wsEndpoint + "/stream?streams=" + strings.Join(streams, "/")
}

func (f *BinanceFeed) Start(ctx context.Context) {
	if len(f.symbols) == 0 {
		return
	}
	go f.run(ctx)
}

func (f *BinanceFeed) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := f.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		f.log.Warn("websocket disconnected; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

type binanceCombined struct {
	Stream string `json:"stream"`
	Data   struct {
		Symbol string `json:"s"`
		Bid    string `json:"b"`
		Ask    string `json:"a"`
	} `json:"data"`
}

func (f *BinanceFeed) streamOnce(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, f.streamURL(), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	connID := fmt.Sprintf("binance-ws-%d", f.connSeq.Add(1))
	f.log.Info("websocket connected", "conn_id", connID, "symbols", len(f.symbols))

	// Binance sends pings; gorilla's default handler pongs. Close on ctx cancel.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	for {
		// Binance disconnects idle streams every 24h; a liquid bookTicker
		// stream ticks far more often than this deadline.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var m binanceCombined
		if err := json.Unmarshal(msg, &m); err != nil || m.Data.Symbol == "" {
			continue
		}
		bid, err1 := strconv.ParseFloat(m.Data.Bid, 64)
		ask, err2 := strconv.ParseFloat(m.Data.Ask, 64)
		if err1 != nil || err2 != nil || bid <= 0 || ask <= 0 {
			continue
		}
		f.cache.set(m.Data.Symbol, Book{
			Bid: bid, Ask: ask, Ts: time.Now(),
			Source: f.wsEndpoint, ConnID: connID,
		})
	}
}
