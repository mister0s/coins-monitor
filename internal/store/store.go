// Package store persists samples in SQLite (modernc.org/sqlite, cgo-free).
// The schema mirrors the JSONL sample fields one-to-one; timestamps are unix
// nanoseconds. Writes are INSERT OR IGNORE against a uniqueness key so the
// JSONL backfill importer is idempotent.
package store

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mister0s/coins-monitor/internal/engine"
)

const schema = `
CREATE TABLE IF NOT EXISTS samples (
	id INTEGER PRIMARY KEY,
	ts INTEGER NOT NULL,
	symbol TEXT NOT NULL,
	tier TEXT NOT NULL DEFAULT '',
	cex_venue TEXT NOT NULL DEFAULT '',
	dex_venue TEXT NOT NULL DEFAULT '',
	chain TEXT NOT NULL DEFAULT '',
	trade_size_usd REAL NOT NULL,
	cex_ts INTEGER NOT NULL DEFAULT 0,
	dex_ts INTEGER NOT NULL DEFAULT 0,
	skew_ms INTEGER NOT NULL DEFAULT 0,
	cex_bid REAL NOT NULL DEFAULT 0,
	cex_ask REAL NOT NULL DEFAULT 0,
	cex_mid REAL NOT NULL DEFAULT 0,
	dex_buy_price REAL NOT NULL DEFAULT 0,
	dex_sell_price REAL NOT NULL DEFAULT 0,
	gross_buy_dex_sell_cex_bps REAL NOT NULL DEFAULT 0,
	net_buy_dex_sell_cex_bps REAL NOT NULL DEFAULT 0,
	gross_buy_cex_sell_dex_bps REAL NOT NULL DEFAULT 0,
	net_buy_cex_sell_dex_bps REAL NOT NULL DEFAULT 0,
	basis_symbol TEXT NOT NULL DEFAULT '',
	basis_mid REAL NOT NULL DEFAULT 0,
	gross_buy_dex_sell_cex_adj_bps REAL NOT NULL DEFAULT 0,
	net_buy_dex_sell_cex_adj_bps REAL NOT NULL DEFAULT 0,
	gross_buy_cex_sell_dex_adj_bps REAL NOT NULL DEFAULT 0,
	net_buy_cex_sell_dex_adj_bps REAL NOT NULL DEFAULT 0,
	cex_mid_chg_10s_bps REAL NOT NULL DEFAULT 0,
	momentum_ok INTEGER NOT NULL DEFAULT 0,
	momentum_bucket TEXT NOT NULL DEFAULT '',
	momentum_threshold_bps REAL NOT NULL DEFAULT 0,
	cex_fee_bps REAL NOT NULL DEFAULT 0,
	gas_usd REAL NOT NULL DEFAULT 0,
	buffer_bps REAL NOT NULL DEFAULT 0,
	pool_tvl_usd REAL NOT NULL DEFAULT 0,
	min_pool_tvl_usd REAL NOT NULL DEFAULT 0,
	include_in_stats INTEGER NOT NULL DEFAULT 0,
	exclude_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_samples_pair_ts ON samples(symbol, ts);
CREATE UNIQUE INDEX IF NOT EXISTS uq_samples_identity ON samples(symbol, trade_size_usd, ts);
`

const sampleColumns = `ts, symbol, tier, cex_venue, dex_venue, chain, trade_size_usd,
	cex_ts, dex_ts, skew_ms, cex_bid, cex_ask, cex_mid, dex_buy_price, dex_sell_price,
	gross_buy_dex_sell_cex_bps, net_buy_dex_sell_cex_bps, gross_buy_cex_sell_dex_bps, net_buy_cex_sell_dex_bps,
	basis_symbol, basis_mid,
	gross_buy_dex_sell_cex_adj_bps, net_buy_dex_sell_cex_adj_bps, gross_buy_cex_sell_dex_adj_bps, net_buy_cex_sell_dex_adj_bps,
	cex_mid_chg_10s_bps, momentum_ok, momentum_bucket, momentum_threshold_bps,
	cex_fee_bps, gas_usd, buffer_bps, pool_tvl_usd, min_pool_tvl_usd, include_in_stats, exclude_reason`

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open creates/opens the database and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// modernc/sqlite is safest with a single writer connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Write inserts one sample (idempotent on the identity key).
func (s *Store) Write(smp engine.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO samples (`+sampleColumns+`) VALUES
		(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		smp.Ts.UnixNano(), smp.Symbol, smp.Tier, smp.CexVenue, smp.DexVenue, smp.Chain, smp.TradeSizeUSD,
		smp.CexTs.UnixNano(), smp.DexTs.UnixNano(), smp.SkewMs,
		smp.CexBid, smp.CexAsk, smp.CexMid, smp.DexBuyPrice, smp.DexSellPrice,
		smp.GrossBuyDexSellCexBps, smp.NetBuyDexSellCexBps, smp.GrossBuyCexSellDexBps, smp.NetBuyCexSellDexBps,
		smp.BasisSymbol, smp.BasisMid,
		smp.GrossBuyDexSellCexAdjBps, smp.NetBuyDexSellCexAdjBps, smp.GrossBuyCexSellDexAdjBps, smp.NetBuyCexSellDexAdjBps,
		smp.CexMidChg10sBps, boolInt(smp.MomentumOK), smp.MomentumBucket, smp.MomentumThresholdBps,
		smp.CexFeeBps, smp.GasUSD, smp.BufferBps, smp.PoolTVLUSD, smp.MinPoolTVLUSD,
		boolInt(smp.IncludeInStats), smp.ExcludeReason)
	return err
}

func scanSample(rows *sql.Rows) (engine.Sample, error) {
	var smp engine.Sample
	var ts, cexTs, dexTs int64
	var momOK, include int
	err := rows.Scan(&ts, &smp.Symbol, &smp.Tier, &smp.CexVenue, &smp.DexVenue, &smp.Chain, &smp.TradeSizeUSD,
		&cexTs, &dexTs, &smp.SkewMs,
		&smp.CexBid, &smp.CexAsk, &smp.CexMid, &smp.DexBuyPrice, &smp.DexSellPrice,
		&smp.GrossBuyDexSellCexBps, &smp.NetBuyDexSellCexBps, &smp.GrossBuyCexSellDexBps, &smp.NetBuyCexSellDexBps,
		&smp.BasisSymbol, &smp.BasisMid,
		&smp.GrossBuyDexSellCexAdjBps, &smp.NetBuyDexSellCexAdjBps, &smp.GrossBuyCexSellDexAdjBps, &smp.NetBuyCexSellDexAdjBps,
		&smp.CexMidChg10sBps, &momOK, &smp.MomentumBucket, &smp.MomentumThresholdBps,
		&smp.CexFeeBps, &smp.GasUSD, &smp.BufferBps, &smp.PoolTVLUSD, &smp.MinPoolTVLUSD,
		&include, &smp.ExcludeReason)
	if err != nil {
		return smp, err
	}
	smp.Ts = time.Unix(0, ts).UTC()
	smp.CexTs = time.Unix(0, cexTs).UTC()
	smp.DexTs = time.Unix(0, dexTs).UTC()
	smp.MomentumOK = momOK != 0
	smp.IncludeInStats = include != 0
	return smp, nil
}

// ForEach streams every sample (oldest first) through fn.
func (s *Store) ForEach(fn func(engine.Sample) error) error {
	rows, err := s.db.Query(`SELECT ` + sampleColumns + ` FROM samples ORDER BY ts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		smp, err := scanSample(rows)
		if err != nil {
			return err
		}
		if err := fn(smp); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SamplesBetween returns one pair's samples in [from, to], ordered by ts.
func (s *Store) SamplesBetween(symbol string, from, to time.Time) ([]engine.Sample, error) {
	rows, err := s.db.Query(`SELECT `+sampleColumns+` FROM samples
		WHERE symbol = ? AND ts BETWEEN ? AND ? ORDER BY ts`,
		symbol, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.Sample
	for rows.Next() {
		smp, err := scanSample(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, smp)
	}
	return out, rows.Err()
}

// Count returns the number of stored samples.
func (s *Store) Count() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&n)
	return n, err
}

// Retain archives every sample older than cutoff into a gzip-compressed
// JSONL file under archiveDir, deletes the archived rows, and VACUUMs.
// Returns the number of archived rows and the archive path ("" if none).
func (s *Store) Retain(cutoff time.Time, archiveDir string) (int64, string, error) {
	rows, err := s.db.Query(`SELECT `+sampleColumns+` FROM samples WHERE ts < ? ORDER BY ts`, cutoff.UnixNano())
	if err != nil {
		return 0, "", err
	}
	var samples []engine.Sample
	for rows.Next() {
		smp, err := scanSample(rows)
		if err != nil {
			rows.Close()
			return 0, "", err
		}
		samples = append(samples, smp)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, "", err
	}
	rows.Close()
	if len(samples) == 0 {
		return 0, "", nil
	}

	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return 0, "", err
	}
	name := fmt.Sprintf("archive-%s.jsonl.gz", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(archiveDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, "", err
	}
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	for _, smp := range samples {
		if err := enc.Encode(smp); err != nil {
			gz.Close()
			f.Close()
			os.Remove(path)
			return 0, "", err
		}
	}
	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(path)
		return 0, "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return 0, "", err
	}

	// Rows are only deleted after the archive file is safely on disk.
	if _, err := s.db.Exec(`DELETE FROM samples WHERE ts < ?`, cutoff.UnixNano()); err != nil {
		return 0, path, fmt.Errorf("archived to %s but delete failed: %w", path, err)
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return int64(len(samples)), path, fmt.Errorf("vacuum: %w", err)
	}
	return int64(len(samples)), path, nil
}
