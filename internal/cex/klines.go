package cex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kline is one Binance candlestick.
type Kline struct {
	OpenTime time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// BinanceKlines fetches candlesticks over REST — an independent transport
// from the websocket feed, used by spotcheck and verify to cross-check
// recorded samples against what the venue reports for the same interval.
func BinanceKlines(ctx context.Context, restEndpoint, symbol, interval string, from, to time.Time) ([]Kline, error) {
	q := url.Values{}
	q.Set("symbol", strings.ToUpper(symbol))
	q.Set("interval", interval)
	q.Set("startTime", strconv.FormatInt(from.UnixMilli(), 10))
	q.Set("endTime", strconv.FormatInt(to.UnixMilli(), 10))
	q.Set("limit", "1000")
	u := strings.TrimRight(restEndpoint, "/") + "/api/v3/klines?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("klines HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rows [][]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	out := make([]Kline, 0, len(rows))
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		openMs, _ := r[0].(float64)
		k := Kline{OpenTime: time.UnixMilli(int64(openMs)).UTC()}
		var errs [5]error
		k.Open, errs[0] = parseF(r[1])
		k.High, errs[1] = parseF(r[2])
		k.Low, errs[2] = parseF(r[3])
		k.Close, errs[3] = parseF(r[4])
		k.Volume, errs[4] = parseF(r[5])
		for _, e := range errs {
			if e != nil {
				return nil, fmt.Errorf("bad kline row: %v", r)
			}
		}
		out = append(out, k)
	}
	return out, nil
}

func parseF(v interface{}) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("not a string: %v", v)
	}
	return strconv.ParseFloat(s, 64)
}
