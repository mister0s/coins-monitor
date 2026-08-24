package evm

import (
	"encoding/hex"
	"math/big"
	"testing"
)

// Known-good selectors from the ERC-20 ABI verify the keccak plumbing.
func TestSelector(t *testing.T) {
	cases := map[string]string{
		"balanceOf(address)":        "70a08231",
		"transfer(address,uint256)": "a9059cbb",
		"decimals()":                "313ce567",
	}
	for sig, want := range cases {
		if got := hex.EncodeToString(Selector(sig)); got != want {
			t.Errorf("Selector(%q) = %s, want %s", sig, got, want)
		}
	}
}

func TestAddressWord(t *testing.T) {
	w, err := AddressWord("0xdAC17F958D2ee523a2206206994597C13D831ec7")
	if err != nil {
		t.Fatal(err)
	}
	want := "000000000000000000000000dac17f958d2ee523a2206206994597c13d831ec7"
	if got := hex.EncodeToString(w); got != want {
		t.Errorf("AddressWord = %s, want %s", got, want)
	}
	if _, err := AddressWord("0x1234"); err == nil {
		t.Error("expected error for short address")
	}
}

func TestUnitsRoundTrip(t *testing.T) {
	cases := []struct {
		amount   float64
		decimals int
	}{
		{1000, 6},
		{0.5, 18},
		{123.456789, 8},
		{20000, 6},
	}
	for _, c := range cases {
		units := ToUnits(c.amount, c.decimals)
		back := FromUnits(units, c.decimals)
		if diff := back/c.amount - 1; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("round trip %v (dec %d): got %v", c.amount, c.decimals, back)
		}
	}
	if got := ToUnits(1000, 6); got.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Errorf("ToUnits(1000, 6) = %s, want 1000000000", got)
	}
}

func TestReadWordAndAddress(t *testing.T) {
	data := make([]byte, 64)
	data[31] = 42
	copy(data[32+12:], mustHex(t, "dac17f958d2ee523a2206206994597c13d831ec7"))
	w, err := ReadWord(data, 0)
	if err != nil || w.Int64() != 42 {
		t.Errorf("ReadWord = %v, %v", w, err)
	}
	addr, err := ReadAddress(data, 1)
	if err != nil || !EqualAddress(addr, "0xdAC17F958D2ee523a2206206994597C13D831ec7") {
		t.Errorf("ReadAddress = %v, %v", addr, err)
	}
	if _, err := ReadWord(data, 2); err == nil {
		t.Error("expected out-of-range error")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
