package dex

import (
	"encoding/hex"
	"math/big"
	"testing"
)

const (
	usdt = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	weth = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	glm  = "0x7DD9c5Cba05E151C895FDe1CF355C9A1D5DA6429"
)

func TestEncodeQuoteSingle(t *testing.T) {
	data, err := encodeQuoteSingle(usdt, weth, big.NewInt(1_000_000), 500)
	if err != nil {
		t.Fatal(err)
	}
	// selector + 5 static words
	if len(data) != 4+5*32 {
		t.Fatalf("len = %d, want %d", len(data), 4+5*32)
	}
	got := hex.EncodeToString(data[:4])
	if got != hex.EncodeToString(selQuoteExactInputSingle) {
		t.Errorf("selector mismatch: %s", got)
	}
	// fee word (index 3 of args)
	feeWord := new(big.Int).SetBytes(data[4+3*32 : 4+4*32])
	if feeWord.Int64() != 500 {
		t.Errorf("fee = %d, want 500", feeWord.Int64())
	}
}

func TestEncodeQuotePath(t *testing.T) {
	tokens := []string{usdt, weth, glm}
	fees := []uint32{500, 3000}
	data, err := encodeQuotePath(tokens, fees, big.NewInt(1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	// path = 20+3+20+3+20 = 66 bytes, padded to 96
	pathLen := 66
	wantLen := 4 + 32 /*offset*/ + 32 /*amountIn*/ + 32 /*bytes len*/ + 96
	if len(data) != wantLen {
		t.Fatalf("len = %d, want %d", len(data), wantLen)
	}
	// offset word must be 64 (bytes arg head starts after two static words)
	off := new(big.Int).SetBytes(data[4:36])
	if off.Int64() != 64 {
		t.Errorf("offset = %d, want 64", off.Int64())
	}
	lenWord := new(big.Int).SetBytes(data[4+64 : 4+96])
	if lenWord.Int64() != int64(pathLen) {
		t.Errorf("bytes length = %d, want %d", lenWord.Int64(), pathLen)
	}
	path := data[4+96 : 4+96+pathLen]
	// fee bytes sit at offsets 20..22 and 43..45
	fee1 := int(path[20])<<16 | int(path[21])<<8 | int(path[22])
	fee2 := int(path[43])<<16 | int(path[44])<<8 | int(path[45])
	if fee1 != 500 || fee2 != 3000 {
		t.Errorf("fees in path = %d, %d; want 500, 3000", fee1, fee2)
	}
}

func TestPathReversal(t *testing.T) {
	u := &UniswapV3{
		quoteToken: usdt,
		baseToken:  glm,
		feeTier:    3000,
		route:      []PathHop{{Token: weth, FeeTier: 500}},
	}
	tokens, fees := u.pathTokensAndFees(true)
	if tokens[0] != usdt || tokens[2] != glm || fees[0] != 500 || fees[1] != 3000 {
		t.Errorf("buy path wrong: %v %v", tokens, fees)
	}
	tokens, fees = u.pathTokensAndFees(false)
	if tokens[0] != glm || tokens[2] != usdt || fees[0] != 3000 || fees[1] != 500 {
		t.Errorf("sell path wrong: %v %v", tokens, fees)
	}
}
