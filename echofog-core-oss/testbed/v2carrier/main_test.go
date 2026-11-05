package main

import (
	"math/bits"
	"testing"
)

// The D1 control must be exactly the traffic GFW's FET rule catches: four set bits per byte on
// average (not 3.3, which OR-ing four random positions gives), no printable runs, no TLS/HTTP.
func TestEncryptedPayloadHasFourDistinctBits(t *testing.T) {
	p := encryptedPayload(600, seeded(42))
	ones, printable, run, longest := 0, 0, 0, 0
	for _, c := range p {
		ones += bits.OnesCount8(c)
		if c >= 0x20 && c <= 0x7e {
			printable++
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	avg := float64(ones) / float64(len(p))
	if avg <= 3.4 || avg >= 4.6 {
		t.Errorf("bits per byte %.2f: the FET rule exempts it", avg)
	}
	if printable > 0 {
		t.Errorf("%d printable bytes: the FET rule exempts text", printable)
	}
	if longest > 20 {
		t.Errorf("a printable run of %d", longest)
	}
}
