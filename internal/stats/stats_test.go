package stats

import (
	"testing"
)

func TestPercentile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	if p := percentile(v, 50); p != 3 {
		t.Errorf("p50 = %v", p)
	}
	if p := percentile(v, 100); p != 5 {
		t.Errorf("p100 = %v", p)
	}
	if p := percentile(v, 0); p != 1 {
		t.Errorf("p0 = %v", p)
	}
}
