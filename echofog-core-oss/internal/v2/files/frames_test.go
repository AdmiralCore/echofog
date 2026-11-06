package files

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func fid(b byte) (f [16]byte) {
	for i := range f {
		f[i] = b + byte(i)
	}
	return
}

func TestWantRoundTrip(t *testing.T) {
	w := Want{FID: fid(1), Ranges: []Range{{Start: 0, Count: 16}, {Start: 100, Count: 1}, {Start: 1 << 40, Count: 200}}}
	b := w.Append(nil)
	if len(b) > 4096 {
		t.Fatalf("a typical WANT is %d bytes, the frame maximum is 4096", len(b))
	}
	got, err := ParseWant(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.FID != w.FID || len(got.Ranges) != 3 || got.Ranges[2] != w.Ranges[2] {
		t.Fatalf("got %+v", got)
	}
	// canonical: encoding the parsed body again gives the same bytes
	if !bytes.Equal(got.Append(nil), b) {
		t.Fatal("not canonical")
	}
}

func TestWantBounds(t *testing.T) {
	f := fid(2)
	enc := func(rs ...Range) []byte { return Want{FID: f, Ranges: rs}.Append(nil) }
	ok := [][]byte{
		enc(Range{0, 256}),
		enc(Range{0, 100}, Range{500, 156}),
	}
	for i, b := range ok {
		if _, err := ParseWant(b); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string][]byte{
		"257 chunks":        enc(Range{0, 257}),
		"257 in two ranges": enc(Range{0, 200}, Range{300, 57}),
		"empty range":       enc(Range{5, 0}),
		"no ranges":         enc(),
		"overflow":          enc(Range{^uint64(0), 2}),
		"short fid":         protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), f[:8]),
		"no fid":            nil,
	}
	for name, b := range bad {
		if _, err := ParseWant(b); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

func TestChunkRoundTripAndBounds(t *testing.T) {
	var p [][32]byte
	for i := 0; i < 5; i++ {
		var h [32]byte
		h[0] = byte(i + 1)
		p = append(p, h)
	}
	c := Chunk{FID: fid(3), Index: 70000, Proof: p, Data: bytes.Repeat([]byte{7}, ChunkSize)}
	b := c.Append(nil)
	if len(b) > 67584 {
		t.Fatalf("a full CHUNK is %d bytes, over the frame maximum", len(b))
	}
	got, err := ParseChunk(b)
	if err != nil || got.Index != 70000 || len(got.Proof) != 5 || got.Proof[4] != p[4] || !bytes.Equal(got.Data, c.Data) {
		t.Fatalf("got %+v %v", got, err)
	}
	// an empty chunk (the only chunk of an empty file) is fine
	if g, err := ParseChunk(Chunk{FID: fid(3)}.Append(nil)); err != nil || len(g.Data) != 0 {
		t.Fatalf("empty chunk: %+v %v", g, err)
	}
	over := Chunk{FID: fid(3), Data: make([]byte, ChunkSize+1)}.Append(nil)
	if _, err := ParseChunk(over); !errors.Is(err, ErrMalformed) {
		t.Errorf("data over a chunk: %v", err)
	}
	many := Chunk{FID: fid(3), Proof: make([][32]byte, MaxProof+1), Data: []byte{1}}.Append(nil)
	if _, err := ParseChunk(many); !errors.Is(err, ErrMalformed) {
		t.Errorf("proof over %d hashes: %v", MaxProof, err)
	}
	// a proof element of the wrong length
	b = protowire.AppendBytes(protowire.AppendTag(Chunk{FID: fid(3)}.Append(nil), 3, protowire.BytesType), make([]byte, 31))
	if _, err := ParseChunk(b); !errors.Is(err, ErrMalformed) {
		t.Errorf("31-byte proof element: %v", err)
	}
}

func TestCancelAndDone(t *testing.T) {
	for _, r := range []uint32{ReasonCancelled, ReasonDeclined, ReasonUnavailable} {
		b := Cancel{FID: fid(4), Reason: r}.Append(nil)
		if len(b) > 32 {
			t.Fatalf("CANCEL is %d bytes, the maximum is 32", len(b))
		}
		got, err := ParseCancel(b)
		if err != nil || got.Reason != r || got.FID != fid(4) {
			t.Fatalf("reason %d: %+v %v", r, got, err)
		}
	}
	if _, err := ParseCancel(Cancel{FID: fid(4), Reason: 4}.Append(nil)); err != nil {
		t.Errorf("an unknown reason must still parse (the caller decides): %v", err)
	}
	b := Done{FID: fid(5)}.Append(nil)
	if len(b) > 32 {
		t.Fatalf("DONE is %d bytes", len(b))
	}
	if d, err := ParseDone(b); err != nil || d.FID != fid(5) {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := ParseDone(nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("DONE without fid: %v", err)
	}
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	b := Done{FID: fid(6)}.Append(nil)
	b = protowire.AppendVarint(protowire.AppendTag(b, 99, protowire.VarintType), 5)
	b = protowire.AppendBytes(protowire.AppendTag(b, 100, protowire.BytesType), []byte("later"))
	if d, err := ParseDone(b); err != nil || d.FID != fid(6) {
		t.Fatalf("%+v %v", d, err)
	}
	// but a known field with the wrong wire type is malformed
	bad := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 5)
	if _, err := ParseDone(bad); !errors.Is(err, ErrMalformed) {
		t.Errorf("fid as varint: %v", err)
	}
}

func FuzzParseFrames(f *testing.F) {
	f.Add(Want{FID: fid(1), Ranges: []Range{{0, 16}}}.Append(nil))
	f.Add(Chunk{FID: fid(1), Index: 3, Proof: [][32]byte{{1}}, Data: []byte("abc")}.Append(nil))
	f.Add(Cancel{FID: fid(1), Reason: 3}.Append(nil))
	f.Add(Done{FID: fid(1)}.Append(nil))
	f.Add([]byte{})
	f.Add([]byte{0x0a, 0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Fuzz(func(t *testing.T, b []byte) {
		if w, err := ParseWant(b); err == nil {
			n := uint64(0)
			for _, r := range w.Ranges {
				n += uint64(r.Count)
				if r.Count == 0 || r.Start+uint64(r.Count) < r.Start {
					t.Fatalf("bad range accepted: %+v", r)
				}
			}
			if n == 0 || n > MaxWantChunks {
				t.Fatalf("%d chunks accepted", n)
			}
		}
		if c, err := ParseChunk(b); err == nil && (len(c.Data) > ChunkSize || len(c.Proof) > MaxProof) {
			t.Fatalf("oversize chunk accepted: %d data, %d proof", len(c.Data), len(c.Proof))
		}
		_, _ = ParseCancel(b)
		_, _ = ParseDone(b)
	})
}
