// Package cex provides read-only top-of-book feeds from centralized exchanges.
package cex

import (
	"context"
	"sync"
	"time"
)

// Book is a top-of-book snapshot for one symbol. Ts is the local receive
// time; Source and ConnID record where the quote physically came from
// (endpoint URL and connection instance) for sample provenance.
type Book struct {
	Bid    float64
	Ask    float64
	Ts     time.Time
	Source string
	ConnID string
}

func (b Book) Mid() float64 { return (b.Bid + b.Ask) / 2 }

// Feed exposes the most recent top-of-book per exchange symbol.
type Feed interface {
	// Book returns the latest snapshot; ok is false if no data has arrived yet.
	Book(symbol string) (Book, bool)
	// ValidateSymbol confirms the symbol is listed on the venue.
	ValidateSymbol(ctx context.Context, symbol string) error
	// Start begins streaming/polling; it returns immediately.
	Start(ctx context.Context)
	Venue() string
}

// bookCache is a small concurrency-safe store shared by feed implementations.
type bookCache struct {
	mu    sync.RWMutex
	books map[string]Book
}

func newBookCache() *bookCache {
	return &bookCache{books: map[string]Book{}}
}

func (c *bookCache) set(symbol string, b Book) {
	c.mu.Lock()
	c.books[symbol] = b
	c.mu.Unlock()
}

func (c *bookCache) get(symbol string) (Book, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.books[symbol]
	return b, ok
}
