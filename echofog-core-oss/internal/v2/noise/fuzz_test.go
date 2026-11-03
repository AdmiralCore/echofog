package noise

import (
	"testing"
)

// FuzzReadMessage feeds arbitrary bytes as each message of both patterns. Run with
//
//	go test -run '^$' -fuzz FuzzReadMessage ./internal/v2/noise/
func FuzzReadMessage(f *testing.F) {
	for _, p := range []*Pattern{XKhfs, XXhfs} {
		pp := newPair(f, p, []byte("seed"))
		msgs := run(f, pp, [3][]byte{[]byte("a"), []byte("b"), []byte("c")})
		for n, m := range msgs {
			f.Add(p == XXhfs, uint8(n), m)
		}
	}
	f.Fuzz(func(t *testing.T, xx bool, step uint8, msg []byte) {
		p := XKhfs
		if xx {
			p = XXhfs
		}
		pp := newPair(t, p, []byte("seed"))
		order := []struct{ w, r *HandshakeState }{{pp.i, pp.r}, {pp.r, pp.i}, {pp.i, pp.r}}
		n := int(step % 3)
		for k := 0; k < n; k++ {
			m, err := order[k].w.WriteMessage(nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := order[k].r.ReadMessage(m); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := order[n].r.ReadMessage(msg); err != nil {
			// After a failure the state refuses further use.
			if _, err := order[n].r.WriteMessage(nil, nil); err == nil {
				t.Fatal("state usable after a failed read")
			}
		}
	})
}
