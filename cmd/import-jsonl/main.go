// Command import-jsonl backfills existing data/*.jsonl sample files into the
// SQLite store, so data collected before the storage migration is not lost.
// The store's identity key makes re-imports idempotent.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mister0s/coins-monitor/internal/engine"
	"github.com/mister0s/coins-monitor/internal/store"
)

func main() {
	dataDir := flag.String("data", "data", "directory of legacy .jsonl sample files")
	dbPath := flag.String("db", "data/monitor.db", "SQLite database to import into")
	flag.Parse()

	if err := run(*dataDir, *dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dataDir, dbPath string) error {
	files, err := filepath.Glob(filepath.Join(dataDir, "*.jsonl"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no .jsonl files found in %s", dataDir)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	before, err := st.Count()
	if err != nil {
		return err
	}
	var read, bad int64
	for _, path := range files {
		r, b, err := importFile(st, path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		read += r
		bad += b
		fmt.Printf("%-40s %8d rows (%d unparseable skipped)\n", filepath.Base(path), r, b)
	}
	after, err := st.Count()
	if err != nil {
		return err
	}
	fmt.Printf("\nimported %d new samples (%d read, %d already present or duplicate) into %s\n",
		after-before, read, read-(after-before), dbPath)
	return nil
}

func importFile(st *store.Store, path string) (read, bad int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) < 2 {
			continue
		}
		var s engine.Sample
		if jerr := json.Unmarshal(sc.Bytes(), &s); jerr != nil || s.Symbol == "" {
			bad++
			continue
		}
		if werr := st.Write(s); werr != nil {
			return read, bad, werr
		}
		read++
	}
	return read, bad, sc.Err()
}
