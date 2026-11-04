package mux

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// rwBuf is a carrier byte stream that yields fixed input and discards output.
type rwBuf struct {
	r *bytes.Reader
}

func (b *rwBuf) Read(p []byte) (int, error)  { return b.r.Read(p) }
func (b *rwBuf) Write(p []byte) (int, error) { return len(p), nil }
func (b *rwBuf) Close() error                { return nil }

func muxSeeds(f *testing.F) {
	var b []byte
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeMuxStream, Payload: frame.MuxStream{StreamID: 0, Data: []byte("hi")}.Append(nil)})
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeMuxWindow, Payload: frame.MuxWindow{StreamID: 0, Increment: 5}.Append(nil)})
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeMuxMaxData, Payload: frame.AppendVarint(nil, 100)})
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeMuxDatagram, Payload: []byte{0x10, 1, 2}})
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeMuxReset, Payload: frame.MuxReset{StreamID: 0, Code: 5}.Append(nil)})
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeMuxStream, Payload: frame.MuxStream{StreamID: 2, Flags: 1}.Append(nil)})
	f.Add(b)
	f.Add([]byte{0x18, 0x00, 0x04, 0x04, 0x01, 0x68, 0x69})
}

// FuzzConnInput feeds arbitrary carrier bytes to the mux decoder. It must never panic or hang,
// and must end with EOF or a violation.
func FuzzConnInput(f *testing.F) {
	muxSeeds(f)
	f.Fuzz(func(t *testing.T, in []byte) {
		c := NewConn(&rwBuf{bytes.NewReader(in)}, ConnConfig{MaxStreams: 8})
		go func() {
			for {
				s, err := c.AcceptStream(t.Context())
				if err != nil {
					return
				}
				go io.Copy(io.Discard, s)
			}
		}()
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("decoder did not finish on finite input")
		}
	})
}

// FuzzRecordConn feeds arbitrary bytes to a sealed record reader.
func FuzzRecordConn(f *testing.F) {
	ik, rk := randomKeys(f)
	snd, _ := NewSealer(ik)
	var w bytes.Buffer
	rc := NewRecordConn(nopCloser{&w})
	rc.StartSealing(snd, 0, 0)
	rc.Write([]byte("a sealed record"))
	f.Add(w.Bytes())
	f.Add([]byte{0, 1, 0})
	f.Add([]byte{0x40, 0x49, 1})
	f.Fuzz(func(t *testing.T, in []byte) {
		rcv, _ := NewSealer(rk)
		r := NewRecordConn(nopCloser{bytes.NewBuffer(in)})
		r.StartSealing(rcv, 0, 0)
		buf := make([]byte, 1024)
		for i := 0; i < 1000; i++ {
			if _, err := r.Read(buf); err != nil {
				return
			}
		}
	})
}

// FuzzSealerOpen: arbitrary sealed datagrams never open and never panic.
func FuzzSealerOpen(f *testing.F) {
	ik, rk := randomKeys(f)
	snd, _ := NewSealer(ik)
	s, _ := snd.Seal(nil, []byte("x"), ClassDatagram, 0, 0)
	f.Add(s)
	f.Fuzz(func(t *testing.T, in []byte) {
		rcv, _ := NewSealer(rk)
		if pt, err := rcv.OpenDatagram(nil, in, 0); err == nil && !bytes.Equal(in, s) {
			t.Fatalf("forged packet opened: %x", pt)
		}
	})
}

// FuzzSessionStreamInput feeds arbitrary frame bytes into a service stream and the control
// stream of a live session. The session must not panic; unknown input never counts as abuse
// unless it is a malformed known frame.
func FuzzSessionStreamInput(f *testing.F) {
	var b []byte
	b = frame.AppendFrame(b, frame.Frame{Type: frame.TypeOpen, Payload: []byte{3, 2, 0}})
	b = frame.AppendFrame(b, frame.Frame{Type: 0x40, Payload: deliver(1, "x")})
	b = frame.AppendFrame(b, frame.Frame{Type: 0x7f, Flags: 1, Payload: []byte("later")})
	f.Add(b, false)
	f.Add(frame.AppendFrame(nil, frame.Frame{Type: frame.TypePing, Payload: make([]byte, 8)}), true)
	f.Fuzz(func(t *testing.T, in []byte, control bool) {
		pr := sessionPair(t, false, nil)
		ctx := ctxT(t)
		if control {
			pr.a.path0Ctrl().w.Write(in)
		} else {
			raw, err := pr.path0.ca.OpenStream(ctx)
			if err != nil {
				return
			}
			raw.Write(in)
			raw.CloseWrite()
			go func() {
				st, err := pr.b.AcceptStream(ctx)
				if err != nil {
					return
				}
				for {
					if _, err := st.ReadFrame(); err != nil {
						return
					}
				}
			}()
		}
		pr.a.Ping(ctx)
	})
}
