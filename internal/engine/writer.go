package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// JSONLWriter appends samples to one JSONL file per pair under dataDir.
type JSONLWriter struct {
	dataDir string
	mu      sync.Mutex
	files   map[string]*os.File
}

func NewJSONLWriter(dataDir string) (*JSONLWriter, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &JSONLWriter{dataDir: dataDir, files: map[string]*os.File{}}, nil
}

func fileNameFor(symbol string) string {
	return strings.ReplaceAll(symbol, "/", "-") + ".jsonl"
}

func (w *JSONLWriter) Write(s Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, ok := w.files[s.Symbol]
	if !ok {
		var err error
		f, err = os.OpenFile(filepath.Join(w.dataDir, fileNameFor(s.Symbol)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		w.files[s.Symbol] = f
	}
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append sample for %s: %w", s.Symbol, err)
	}
	return nil
}

func (w *JSONLWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var firstErr error
	for _, f := range w.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	w.files = map[string]*os.File{}
	return firstErr
}
