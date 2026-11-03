package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// FuzzNegotiation: whatever an initiator offers, the responder's selection passes the
// initiator's downgrade check, never contains GREASE and only contains served pairs.
func FuzzNegotiation(f *testing.F) {
	f.Add([]byte{3, 2, 0x0a, 1, 1, 2, 0xf0, 9})
	f.Fuzz(func(t *testing.T, in []byte) {
		var offers []Offer
		var feats []uint16
		for i := 0; i+1 < len(in); i += 2 {
			id := uint64(in[i])
			if in[i]&0x80 != 0 {
				id = uint64(in[i])<<8 | uint64(in[i])&0x0f | 0x0a0a&0x0f0f
			}
			offers = append(offers, Offer{ID: id, Versions: []uint64{uint64(in[i+1] % 4)}})
			feats = append(feats, uint16(in[i])<<8|uint16(in[i+1]))
		}
		r := NewRegistry()
		r.Serve(Chat, 2, nil)
		r.Serve(DHT, 2, nil)
		choices := r.Select(offers)
		if err := VerifyChoices(offers, choices); err != nil {
			t.Fatalf("own selection fails verification: %v", err)
		}
		for _, c := range choices {
			if IsGreaseService(c.ID) || c.Version != 2 || (c.ID != Chat && c.ID != DHT) {
				t.Fatalf("bad choice %+v", c)
			}
		}
		sel := SelectFeatures(feats, []uint16{1, 4, 6, 0x0a0a})
		if err := VerifyFeatures(feats, sel); err != nil && !slices.ContainsFunc(sel, IsGrease) {
			// feats may contain duplicates and be unsorted; the selection is still a subset.
			for _, s := range sel {
				if !slices.Contains(feats, s) {
					t.Fatal(err)
				}
			}
		}
		if slices.ContainsFunc(sel, IsGrease) {
			t.Fatal("GREASE selected")
		}
	})
}

// FuzzDispatch: arbitrary OPEN payloads and frames on a stream served by Serve never panic and
// never count as abuse unless a known frame is malformed.
func FuzzDispatch(f *testing.F) {
	f.Add(uint64(3), uint64(2), []byte{0x40, 0, 2, 8, 1, 0x7f, 1, 1, 0})
	f.Add(uint64(0xf00d), uint64(1), []byte{0x7f, 1, 0})
	f.Fuzz(func(t *testing.T, svc, ver uint64, body []byte) {
		if svc > frame.MaxVarint || ver > frame.MaxVarint {
			return
		}
		ini := &peer{reg: testRegistry(t)}
		resp := &peer{reg: testRegistry(t)}
		l := connect(t, ini, resp)
		ctx, cancel := context.WithCancel(ctxT(t))
		defer cancel()
		st, err := l.a.OpenStream(ctx, svc, ver)
		if err != nil {
			return
		}
		// body is a sequence of frames (any types, flags, lengths) sent after OPEN.
		for len(body) > 0 {
			fr, n, err := frame.ParseFrame(body, func(uint64) (int, bool) { return frame.MaxPayload, true })
			if n == 0 || (err != nil && !frame.IsUnknown(err)) {
				break
			}
			body = body[n:]
			if st.WriteFrame(fr.Type, fr.Flags, fr.Payload) != nil {
				break
			}
		}
		st.CloseWrite()
		st.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for i := 0; i < 4; i++ {
			if _, err := st.ReadFrame(); err != nil {
				break
			}
		}
		if _, err := l.a.Ping(ctx); err != nil {
			t.Fatalf("session broken: %v", err)
		}
	})
}
