package evm

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
)

// fakeRPC answers eth_call with a canned mapping from calldata to result.
func fakeRPC(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad rpc request: %v", err)
			return
		}
		if req.Method != "eth_call" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"unknown method"}}`, req.ID)
			return
		}
		var call struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(req.Params[0], &call); err != nil {
			t.Errorf("bad call params: %v", err)
			return
		}
		result, ok := responses[strings.ToLower(call.Data)]
		if !ok {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":3,"message":"execution reverted"}}`, req.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":"%s"}`, req.ID, result)
	}))
}

func word(v int64) string {
	return hex.EncodeToString(Word(big.NewInt(v)))
}

func TestClientCallRoundTrip(t *testing.T) {
	token := "0xdac17f958d2ee523a2206206994597c13d831ec7"
	decimalsData := "0x" + hex.EncodeToString(Selector("decimals()"))
	srv := fakeRPC(t, map[string]string{
		decimalsData: "0x" + word(6),
	})
	defer srv.Close()

	c := NewClient(srv.URL)
	d, err := c.ERC20Decimals(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if d != 6 {
		t.Errorf("decimals = %d, want 6", d)
	}

	// Unknown calldata must surface the revert as an error.
	if _, err := c.ERC20BalanceOf(context.Background(), token, token); err == nil {
		t.Error("expected revert error")
	}
}
