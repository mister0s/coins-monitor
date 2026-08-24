package dex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJupiterQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("swapMode") != "ExactIn" {
			t.Errorf("swapMode = %q", q.Get("swapMode"))
		}
		switch {
		case q.Get("inputMint") == "USDTMINT" && q.Get("amount") == "1000000000":
			// $1000 in (6 dp) -> 5.0 SOL out (9 dp): effective price $200/SOL
			fmt.Fprint(w, `{"outAmount":"5000000000"}`)
		case q.Get("inputMint") == "SOLMINT" && q.Get("amount") == "5000000000":
			// 5 SOL in -> $995 out
			fmt.Fprint(w, `{"outAmount":"995000000"}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"no route"}`)
		}
	}))
	defer srv.Close()

	j, err := NewJupiter(srv.URL, "SOLMINT", 9, "USDTMINT", 6)
	if err != nil {
		t.Fatal(err)
	}
	baseOut, err := j.QuoteBuyBase(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if baseOut != 5.0 {
		t.Errorf("baseOut = %v, want 5.0", baseOut)
	}
	quoteOut, err := j.QuoteSellBase(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if quoteOut != 995.0 {
		t.Errorf("quoteOut = %v, want 995", quoteOut)
	}
	if _, err := j.QuoteBuyBase(context.Background(), 42); err == nil {
		t.Error("expected error for unroutable amount")
	}
	if _, err := j.PoolTVLUSD(context.Background(), 200); err != ErrTVLUnsupported {
		t.Errorf("expected ErrTVLUnsupported, got %v", err)
	}
}
