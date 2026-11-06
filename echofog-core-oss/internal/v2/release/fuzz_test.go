package release_test

import (
	"testing"
	"time"

	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/release"
)

// FuzzOffer: a manifest comes from the DHT, a pex answer or a web page, so from anybody. Offer and
// Check must not panic, must not apply anything that does not pass objects.ReleaseState, and a
// refused input must leave the state untouched.
func FuzzOffer(f *testing.F) {
	r := &rig{t: f, clk: newClock(), kv: newKV()}
	for i := byte(1); i <= 3; i++ {
		r.k = append(r.k, signer(f, i))
	}
	good := r.seal("0.3.1", time.Hour, proto(200, 210), r.k[0], r.k[1])
	f.Add(good)
	f.Add(r.seal("0.3.1", time.Hour, proto(200, 210), r.k[0]))
	f.Add(r.seal("0.3.1", time.Hour, func(rel *pb.Release) {
		rel.NextKeys = [][]byte{{1}, {2, 3}}
		rel.RevokedKeys = [][]byte{{0xff}}
	}, r.k[0], r.k[1]))
	f.Add(good[:len(good)/2])
	f.Add([]byte{})
	f.Add(make([]byte, release.MaxManifestSize+1))

	f.Fuzz(func(t *testing.T, raw []byte) {
		fr := &rig{t: t, clk: newClock(), kv: newKV(), k: r.k}
		m := fr.open()
		ok, err := m.Offer(raw)
		if err != nil || !ok {
			if m.Latest() != nil || m.Last() != nil {
				t.Fatal("a refused manifest changed the state")
			}
			if fr.kv.count("m/") != 0 {
				t.Fatal("a refused manifest was persisted")
			}
			return
		}
		if m.Last() == nil || len(m.Latest()) == 0 {
			t.Fatal("applied without a current manifest")
		}
	})
}
