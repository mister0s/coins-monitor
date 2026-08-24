package stats

import (
	"github.com/mister0s/coins-monitor/internal/engine"
	"github.com/mister0s/coins-monitor/internal/store"
)

// LoadStore aggregates every sample in the SQLite store.
func LoadStore(st *store.Store, includeExcluded bool) (map[Key]*Agg, error) {
	aggs := map[Key]*Agg{}
	err := st.ForEach(func(s engine.Sample) error {
		record(aggs, s, includeExcluded)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return aggs, nil
}
