package behaviour

import (
	"strings"
	"testing"
	"time"
)

// A censor description comes from a file somebody wrote: it must never panic, and what loads must
// be something the model can run.
func FuzzLoadCensorConfig(f *testing.F) {
	f.Add(`{"freeze_bytes":16384,"udp_drop":true}`)
	f.Add(`{"allow_cidr":["203.0.113.0/24"],"allow_sni":["a"],"only_443":true}`)
	f.Add(`{"and":{"asns":["A"],"ja4s":["j"],"parallel":3,"penalty_seconds":120}}`)
	f.Add(`{"shutdown_from":"2026-11-01T00:00:00Z","shutdown_to":"2026-11-01T01:00:00Z"}`)
	f.Add(`{`)
	f.Fuzz(func(t *testing.T, spec string) {
		c, err := LoadCensorConfig(strings.NewReader(spec))
		if err != nil {
			return
		}
		if c.FreezeBytes < 0 || c.ThrottleBps < 0 {
			t.Fatalf("a negative size got through: %+v", c)
		}
		if c.And != nil && (c.And.Parallel < 1 || c.And.Penalty <= 0) {
			t.Fatalf("an AND rule that can never or always fire: %+v", c.And)
		}
		// whatever loaded can be played
		clk := NewVClock()
		cen := NewCensor(c, clk)
		a := Attempt{Flow: 1, Proto: "tcp", Dst: dstA, SNI: "a", JA4: "j", ASN: "A"}
		cen.Begin(a)
		clk.Advance(time.Second)
		cen.Judge(a)
		cen.End(a)
	})
}
