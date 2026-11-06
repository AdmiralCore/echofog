package probe

import (
	"strings"
	"testing"
)

func FuzzLoadSpec(f *testing.F) {
	f.Add(`[{"scenario":"ordinary","addr":"a:1","cover":"b:1"}]`)
	f.Add(`[{"scenario":"hidden","network":"udp","addr":"a:1","first_flight":"00ff"}]`)
	f.Add(`{`)
	f.Fuzz(func(t *testing.T, spec string) {
		cases, err := LoadSpec(strings.NewReader(spec))
		if err != nil {
			return
		}
		if len(cases) == 0 {
			t.Fatal("a spec that loads has at least one case")
		}
		for _, c := range cases {
			if c.Target.Addr == "" || (c.Target.Network != "tcp" && c.Target.Network != "udp") {
				t.Fatalf("a case that cannot run was accepted: %+v", c.Target)
			}
		}
	})
}

func FuzzReduce(f *testing.F) {
	f.Add("tcp", []byte{0x15, 3, 3, 0, 2, 2, 70})
	f.Add("udp", []byte{0xc3, 0, 0, 0, 1})
	f.Add("tcp", []byte("HTTP/1.1 200 OK"))
	f.Fuzz(func(t *testing.T, network string, got []byte) {
		a := Reduce(network, got, nil)
		if a.Class == "" {
			t.Fatal("every answer has a class")
		}
		if !Equivalent(a, Reduce(network, got, nil)) {
			t.Fatal("the same bytes must reduce to the same answer")
		}
	})
}
