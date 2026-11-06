package censor

import (
	"bytes"
	"testing"
)

// The decoders of the launchpad read what a capture or a peer hands them: none may panic, and a
// stream read in pieces must give what the same bytes read at once give.

func FuzzAnalyzeReader(f *testing.F) {
	f.Add(exchangePcap().buf.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0xd4, 0xc3, 0xb2, 0xa1, 2, 0, 4, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		flows, err := AnalyzeReader(bytes.NewReader(data))
		if err != nil {
			return
		}
		for _, fl := range flows {
			if len(fl.Pkts) > maxPkts {
				t.Fatalf("%d packets kept, the bound is %d", len(fl.Pkts), maxPkts)
			}
			for _, r := range fl.Records {
				if r.Len < 5 {
					t.Fatalf("a record of %d bytes", r.Len)
				}
			}
		}
	})
}

func FuzzRecordScanner(f *testing.F) {
	f.Add([]byte{22, 3, 3, 0, 4, 1, 2, 3, 4, 23, 3, 3, 0, 1, 9})
	f.Add([]byte{})
	f.Add([]byte{22, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		var whole, pieces []Record
		var a, b recordScanner
		a.feed(data, 0, true, &whole)
		for i := 0; i < len(data); i += 3 {
			b.feed(data[i:min(i+3, len(data))], 0, true, &pieces)
		}
		if len(whole) != len(pieces) {
			t.Fatalf("whole %d records, in pieces %d", len(whole), len(pieces))
		}
		for i := range whole {
			if whole[i] != pieces[i] {
				t.Fatalf("record %d differs: %+v against %+v", i, whole[i], pieces[i])
			}
		}
	})
}
