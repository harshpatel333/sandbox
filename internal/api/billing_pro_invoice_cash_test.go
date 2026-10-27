package api

import (
	"math/big"
	"testing"
)

func TestProProtectedUsageConservativeRounding(t *testing.T) {
	for _, tc := range []struct {
		name, total, protected string
		billed, want           int64
	}{
		{"subcent charge rounds to zero", "0.4", "0.4", 0, 0},
		{"subcent part rounds conservatively", "100.4", "0.4", 100, 1},
		{"negative correction caps protection", "600", "500", 100, 100},
		{"positive correction is protected", "600", "0", 650, 50},
		{"pro overage with protected use", "30002", "2", 30002, 2},
		{"only paid usage", "30000", "0", 30000, 0},
		{"negative correction never creates credit", "600", "2", 100, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := new(big.Rat).SetString(tc.total)
			b, _ := new(big.Rat).SetString(tc.protected)
			got, err := protectedUsageCents(a, b, tc.billed)
			if err != nil || got != tc.want {
				t.Fatalf("got %d %v want%d", got, err, tc.want)
			}
		})
	}
	if _, err := protectedUsageCents(big.NewRat(1, 1), big.NewRat(2, 1), 1); err == nil {
		t.Fatal("invalid temporal split accepted")
	}
}
