package main

import (
	"testing"
)

func FuzzParsePricePerGB(f *testing.F) {
	for _, s := range []string{"0", "0.05", "1000", "1e3", "-1", "NaN", "Inf", "", " 0.5 ", "1,5", "0x10", "9999999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := parsePricePerGB(s)
		if err == nil && (v < 0 || float64(v) > 1000*1e6/1024+1) {
			t.Fatalf("price %q became %d micro-USDT per MB", s, v)
		}
	})
}
