// Package evm is a minimal read-only JSON-RPC client with just enough ABI
// encoding to quote DEX swaps via eth_call. It deliberately supports nothing
// that could sign or send a transaction.
package evm

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/mister0s/coins-monitor/internal/ratelimit"
)

type Client struct {
	url     string
	hc      *http.Client
	id      atomic.Int64
	limiter *ratelimit.Limiter // nil = unlimited
}

// NewClient creates a read-only JSON-RPC client. A non-nil limiter paces all
// requests through this client (shared across every pair on the chain).
func NewClient(rpcURL string, limiter *ratelimit.Limiter) *Client {
	return &Client{
		url:     rpcURL,
		hc:      &http.Client{Timeout: 15 * time.Second},
		limiter: limiter,
	}
}

// URL returns the RPC endpoint this client talks to.
func (c *Client) URL() string { return c.url }

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int64         `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Call performs eth_call against `to` with the given calldata at the latest block.
func (c *Client) Call(ctx context.Context, to string, data []byte) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.id.Add(1),
		Method:  "eth_call",
		Params: []interface{}{
			map[string]string{"to": to, "data": "0x" + hex.EncodeToString(data)},
			"latest",
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("rpc decode (http %d): %w", resp.StatusCode, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", out.Error.Code, out.Error.Message)
	}
	var hexResult string
	if err := json.Unmarshal(out.Result, &hexResult); err != nil {
		return nil, fmt.Errorf("rpc result decode: %w", err)
	}
	return hex.DecodeString(strings.TrimPrefix(hexResult, "0x"))
}

// ---- ABI helpers ----

// Selector returns the 4-byte function selector for a canonical signature,
// e.g. Selector("balanceOf(address)").
func Selector(sig string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(sig))
	return h.Sum(nil)[:4]
}

// Word pads a big.Int to a 32-byte ABI word (unsigned, big-endian).
func Word(v *big.Int) []byte {
	b := make([]byte, 32)
	v.FillBytes(b)
	return b
}

// AddressWord pads a 20-byte address into a 32-byte ABI word.
func AddressWord(addr string) ([]byte, error) {
	raw, err := ParseAddress(addr)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	copy(b[12:], raw)
	return b, nil
}

// BoolWord encodes a bool as a 32-byte ABI word.
func BoolWord(v bool) []byte {
	b := make([]byte, 32)
	if v {
		b[31] = 1
	}
	return b
}

// ParseAddress decodes a 0x-prefixed 20-byte hex address.
func ParseAddress(addr string) ([]byte, error) {
	s := strings.TrimPrefix(strings.TrimSpace(addr), "0x")
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 20 {
		return nil, fmt.Errorf("invalid address %q", addr)
	}
	return raw, nil
}

// ReadWord returns the i-th 32-byte word of an ABI-encoded return blob as a big.Int.
func ReadWord(data []byte, i int) (*big.Int, error) {
	if len(data) < (i+1)*32 {
		return nil, fmt.Errorf("abi return too short: want word %d, have %d bytes", i, len(data))
	}
	return new(big.Int).SetBytes(data[i*32 : (i+1)*32]), nil
}

// ReadAddress returns the i-th return word interpreted as an address.
func ReadAddress(data []byte, i int) (string, error) {
	w, err := ReadWord(data, i)
	if err != nil {
		return "", err
	}
	b := make([]byte, 32)
	w.FillBytes(b)
	return "0x" + hex.EncodeToString(b[12:]), nil
}

// EqualAddress compares two hex addresses case-insensitively.
func EqualAddress(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(a, "0x"), strings.TrimPrefix(b, "0x"))
}

// ---- common ERC-20 reads ----

var (
	selDecimals  = Selector("decimals()")
	selBalanceOf = Selector("balanceOf(address)")
)

func (c *Client) ERC20Decimals(ctx context.Context, token string) (int, error) {
	out, err := c.Call(ctx, token, selDecimals)
	if err != nil {
		return 0, fmt.Errorf("decimals(%s): %w", token, err)
	}
	w, err := ReadWord(out, 0)
	if err != nil {
		return 0, err
	}
	d := int(w.Int64())
	if d < 0 || d > 36 {
		return 0, fmt.Errorf("implausible decimals %d for %s", d, token)
	}
	return d, nil
}

func (c *Client) ERC20BalanceOf(ctx context.Context, token, holder string) (*big.Int, error) {
	hw, err := AddressWord(holder)
	if err != nil {
		return nil, err
	}
	data := append(append([]byte{}, selBalanceOf...), hw...)
	out, err := c.Call(ctx, token, data)
	if err != nil {
		return nil, fmt.Errorf("balanceOf(%s) on %s: %w", holder, token, err)
	}
	return ReadWord(out, 0)
}

// ---- unit conversion ----

// ToUnits converts a human amount to integer token units (amount * 10^decimals).
func ToUnits(amount float64, decimals int) *big.Int {
	f := new(big.Float).SetPrec(128).SetFloat64(amount)
	scale := new(big.Float).SetPrec(128).SetInt(pow10(decimals))
	f.Mul(f, scale)
	i, _ := f.Int(nil)
	return i
}

// FromUnits converts integer token units back to a human amount.
func FromUnits(units *big.Int, decimals int) float64 {
	f := new(big.Float).SetPrec(128).SetInt(units)
	scale := new(big.Float).SetPrec(128).SetInt(pow10(decimals))
	f.Quo(f, scale)
	v, _ := f.Float64()
	return v
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
