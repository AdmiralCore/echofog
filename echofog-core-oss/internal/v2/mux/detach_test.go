package mux

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

// L8 (T6.1 accept 1): Detach hands over the byte stream exactly after the last frame read: no
// byte is lost to read-ahead, and writes through Detached from several goroutines stay whole.
func TestDetachLosesNoBytesAfterFrames(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		pr := sessionPair(t, sealed, nil)
		ctx := ctxT(t)
		st, err := pr.a.OpenStream(ctx, 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		raw := make([]byte, 50_000)
		for i := range raw {
			raw[i] = byte(i * 7)
		}
		if err := st.WriteFrame(0x40, 0, deliver(1, "before")); err != nil {
			t.Fatal(err)
		}
		d := st.Detach()
		if _, err := d.Write(raw); err != nil {
			t.Fatal(err)
		}
		_ = d.CloseWrite()

		rs, err := pr.b.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		f, err := rs.ReadFrame()
		if err != nil || f.Type != 0x40 {
			t.Fatalf("frame before detach: %+v %v", f, err)
		}
		got, err := io.ReadAll(rs.Detach())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("sealed=%v: detached bytes differ: got %d, want %d", sealed, len(got), len(raw))
		}
	}
}

func TestDetachedWritesAreWhole(t *testing.T) {
	pr := sessionPair(t, true, nil)
	ctx := ctxT(t)
	st, err := pr.a.OpenStream(ctx, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(0x40, 0, deliver(1, "x")); err != nil {
		t.Fatal(err)
	}
	rs, err := pr.b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	const chunk, per = 1000, 60
	d := st.Detach()
	var wg sync.WaitGroup
	for g := byte(1); g <= 3; g++ {
		wg.Add(1)
		go func(g byte) {
			defer wg.Done()
			b := bytes.Repeat([]byte{g}, chunk)
			for i := 0; i < per; i++ {
				if _, err := d.Write(b); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	go func() { wg.Wait(); _ = d.CloseWrite() }()
	got, err := io.ReadAll(rs.Detach())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3*chunk*per {
		t.Fatalf("got %d bytes, want %d", len(got), 3*chunk*per)
	}
	for i := 0; i < len(got); i += chunk {
		if !bytes.Equal(got[i:i+chunk], bytes.Repeat([]byte{got[i]}, chunk)) {
			t.Fatalf("chunk at %d interleaved with another writer", i)
		}
	}
}
