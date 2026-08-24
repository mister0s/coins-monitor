package dex

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mister0s/coins-monitor/internal/evm"
)

const (
	v2Router  = "0x7a250d5630b4cf539739df2c5dacb4c659f2488d"
	v2Factory = "0x5c69bee701ef814a2b6a3edd4b1652cb9cc5aa6f"
	v2Pair    = "0xf8b8dfd07ad76c92b29683613e6c8dc654803a2f"
)

func hexWord(v *big.Int) string { return hex.EncodeToString(evm.Word(v)) }

func hexAddr(t *testing.T, a string) string {
	t.Helper()
	w, err := evm.AddressWord(a)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(w)
}

// fakeV2RPC answers eth_call by (to, selector) prefix.
func fakeV2RPC(t *testing.T, handlers map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64             `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		var call struct{ To, Data string }
		if err := json.Unmarshal(req.Params[0], &call); err != nil {
			t.Fatal(err)
		}
		key := strings.ToLower(call.To) + ":" + strings.ToLower(call.Data[:10])
		result, ok := handlers[key]
		if !ok {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":3,"message":"execution reverted (%s)"}}`, req.ID, key)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":"0x%s"}`, req.ID, result)
	}))
}

func v2Handlers(t *testing.T) map[string]string {
	sel := func(b []byte) string { return "0x" + hex.EncodeToString(b) }
	// getAmountsOut result for a 3-token path: [offset][len=3][a0][a1][a2]
	amounts := hexWord(big.NewInt(32)) + hexWord(big.NewInt(3)) +
		hexWord(big.NewInt(1_000_000)) + hexWord(big.NewInt(500)) +
		hexWord(big.NewInt(5_000_000_000_000_000_000)) // 5.0 base out (18 dp)
	reserves := hexWord(big.NewInt(2_000_000_000_000_000_000)) + // r0: 2.0 base
		hexWord(big.NewInt(9_000_000)) + hexWord(big.NewInt(0)) // r1, blockTimestampLast
	return map[string]string{
		v2Router + ":" + sel(selFactory):       hexAddr(t, v2Factory),
		v2Factory + ":" + sel(selGetPair):      hexAddr(t, v2Pair),
		v2Pair + ":" + sel(selToken0):          hexAddr(t, glm), // base is token0
		v2Router + ":" + sel(selGetAmountsOut): amounts,
		v2Pair + ":" + sel(selGetReserves):     reserves,
	}
}

func newTestV2(t *testing.T, srvURL, pool string) (*UniswapV2, error) {
	t.Helper()
	client := evm.NewClient(srvURL, nil)
	return NewUniswapV2(context.Background(), client, v2Router, pool,
		glm, 18, usdt, 6, []string{weth})
}

func TestUniswapV2QuoteAndDerivedPool(t *testing.T) {
	srv := fakeV2RPC(t, v2Handlers(t))
	defer srv.Close()

	u, err := newTestV2(t, srv.URL, "") // pool derived from factory
	if err != nil {
		t.Fatal(err)
	}
	if !evm.EqualAddress(u.pool, v2Pair) {
		t.Errorf("derived pool = %s, want %s", u.pool, v2Pair)
	}
	baseOut, err := u.QuoteBuyBase(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if baseOut != 5.0 {
		t.Errorf("baseOut = %v, want 5.0 (last element of amounts)", baseOut)
	}
	// Routed pair: TVL prices only the base reserve.
	tvl, err := u.PoolTVLUSD(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if tvl != 200 { // 2.0 base * $100
		t.Errorf("tvl = %v, want 200", tvl)
	}
}

func TestUniswapV2RejectsFakePool(t *testing.T) {
	srv := fakeV2RPC(t, v2Handlers(t))
	defer srv.Close()

	_, err := newTestV2(t, srv.URL, "0x000000000000000000000000000000000000dead")
	if err == nil || !strings.Contains(err.Error(), "does not match factory pair") {
		t.Errorf("fake pool must be rejected with a factory mismatch, got: %v", err)
	}
	// The genuine factory-derived address passes.
	if _, err := newTestV2(t, srv.URL, v2Pair); err != nil {
		t.Errorf("genuine pair rejected: %v", err)
	}
}

func TestUniswapV2PathDirection(t *testing.T) {
	u := &UniswapV2{quoteToken: usdt, baseToken: glm, route: []string{weth}}
	buy := u.path(true)
	sell := u.path(false)
	if buy[0] != usdt || buy[2] != glm || sell[0] != glm || sell[2] != usdt {
		t.Errorf("paths wrong: buy=%v sell=%v", buy, sell)
	}
}
