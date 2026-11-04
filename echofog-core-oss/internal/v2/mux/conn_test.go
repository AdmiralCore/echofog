package mux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

func connPair(t testing.TB, ccfg, scfg ConnConfig) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	ccfg.Client = true
	scfg.Client = false
	c := NewConn(a, ccfg)
	s := NewConn(b, scfg)
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

func ctxT(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestConnStreamRoundTrip(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	ids := []uint64{}
	for i := 0; i < 3; i++ {
		st, err := c.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, st.ID())
		if _, err := st.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		st.CloseWrite()
	}
	if ids[0] != 0 || ids[1] != 2 || ids[2] != 4 {
		t.Fatalf("client stream IDs %v, want 0 2 4", ids)
	}
	for i := 0; i < 3; i++ {
		st, err := s.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(st)
		if err != nil || string(b) != "hello" || st.ID() != ids[i] {
			t.Fatalf("stream %d: %q %v", st.ID(), b, err)
		}
		// The server answers on the same stream.
		st.Write([]byte("bye"))
		st.CloseWrite()
	}
	st, _ := s.OpenStream(ctx)
	if st.ID() != 1 {
		t.Fatalf("server stream ID %d, want 1", st.ID())
	}
}

func TestConnLargeTransferBothWays(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<17) // 2 MiB: needs window updates
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		st, _ := c.OpenStream(ctx)
		st.Write(data)
		st.CloseWrite()
		got, err := io.ReadAll(st)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("client read %d bytes, %v", len(got), err)
		}
	}()
	go func() {
		defer wg.Done()
		st, err := s.AcceptStream(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		// Echo while reading: both directions are in flight at once.
		got, err := io.ReadAll(st)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("server read %d bytes, %v", len(got), err)
		}
		st.Write(got)
		st.CloseWrite()
	}()
	wg.Wait()
}

func TestConnBackpressureStreamWindow(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	st, _ := c.OpenStream(ctx)
	st.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := st.Write(make([]byte, 1<<20))
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("write did not block: n=%d err=%v", n, err)
	}
	if n != DefaultStreamWindow {
		t.Fatalf("wrote %d bytes before blocking, want exactly the stream window %d", n, DefaultStreamWindow)
	}
	// The reader consumes; the writer may continue.
	rs, err := s.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, rs)
	st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.Write(make([]byte, 1<<20)); err != nil {
		t.Fatalf("write after the reader drained: %v", err)
	}
}

func TestConnBackpressureConnectionWindow(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	total := 0
	for i := 0; i < 20; i++ { // 20 × 256 KiB = 5 MiB > 4 MiB connection window
		st, _ := c.OpenStream(ctx)
		st.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		n, _ := st.Write(make([]byte, DefaultStreamWindow))
		total += n
	}
	if total != DefaultConnWindow {
		t.Fatalf("wrote %d bytes over all streams, want the connection window %d", total, DefaultConnWindow)
	}
	// Draining one stream frees connection credit (MUX_MAX_DATA).
	for i := 0; i < 8; i++ {
		rs, err := s.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, rs)
	}
	st, _ := c.OpenStream(ctx)
	st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.Write(make([]byte, 100000)); err != nil {
		t.Fatalf("write after drain: %v", err)
	}
}

func TestConnReset(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	st, _ := c.OpenStream(ctx)
	st.Write([]byte("before"))
	st.Reset(frame.CodeRefused)
	rs, err := s.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 100)
	n, err := rs.Read(b)
	if err != nil || string(b[:n]) != "before" {
		t.Fatalf("data before the reset: %q %v", b[:n], err)
	}
	_, err = rs.Read(b)
	var re *ResetError
	if !errors.As(err, &re) || re.Code != frame.CodeRefused || !re.Remote {
		t.Fatalf("read after reset: %v", err)
	}
	if _, err := rs.Write([]byte("x")); !errors.As(err, &re) {
		t.Fatalf("write after reset: %v", err)
	}
	if _, err := st.Write([]byte("x")); !errors.As(err, &re) || re.Remote {
		t.Fatalf("local write after local reset: %v", err)
	}
	// The connection survives a stream reset.
	st2, _ := c.OpenStream(ctx)
	st2.Write([]byte("ok"))
	st2.CloseWrite()
	rs2, _ := s.AcceptStream(ctx)
	if got, _ := io.ReadAll(rs2); string(got) != "ok" {
		t.Fatalf("after reset: %q", got)
	}
}

func TestConnResetReturnsConnectionCredit(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	// Fill 16 streams (the whole 4 MiB) and have the receiver reset them unread.
	for i := 0; i < 16; i++ {
		st, _ := c.OpenStream(ctx)
		st.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := st.Write(make([]byte, DefaultStreamWindow)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 16; i++ {
		rs, _ := s.AcceptStream(ctx)
		for {
			s.mu.Lock()
			full := uint64(len(rs.(*connStream).buf)) == DefaultStreamWindow
			s.mu.Unlock()
			if full {
				break
			}
			time.Sleep(time.Millisecond)
		}
		rs.Reset(frame.CodeRefused)
	}
	st, _ := c.OpenStream(ctx)
	st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.Write(make([]byte, 1000)); err != nil {
		t.Fatalf("connection credit was not returned by reset: %v", err)
	}
}

func TestConnDatagrams(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{DatagramQueue: 4})
	ctx := ctxT(t)
	d := frame.AppendDatagram(nil, frame.DgramCallAudio, []byte("media"))
	if err := c.SendDatagram(d); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, d) {
		t.Fatalf("%x %v", got, err)
	}
	if err := c.SendDatagram(make([]byte, MaxDatagram+1)); err != ErrDatagramTooLarge {
		t.Fatalf("oversized datagram: %v", err)
	}
	// The receiver's queue holds 4; the rest is dropped and counted, never an error.
	for i := 0; i < 20; i++ {
		c.SendDatagram(d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Stats().DatagramsDroppedIn == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Stats().DatagramsDroppedIn == 0 {
		t.Fatal("full receive queue did not drop")
	}
	if c.Err() != nil || s.Err() != nil {
		t.Fatal("dropping datagrams closed the connection")
	}
}

// rawPeer is the far end of a pipe that speaks raw frames to a Conn under test. With the
// default config the Conn is the carrier server, so the raw peer opens even stream IDs.
type rawPeer struct {
	t      testing.TB
	conn   net.Conn
	frames chan frame.Frame
}

func newRawPeer(t testing.TB, cfg ConnConfig) (*Conn, *rawPeer) {
	a, b := net.Pipe()
	c := NewConn(a, cfg)
	p := &rawPeer{t: t, conn: b, frames: make(chan frame.Frame, 4096)}
	go func() {
		rd := frame.NewReader(b, nil)
		for {
			f, err := rd.ReadFrame()
			if err != nil {
				close(p.frames)
				return
			}
			p.frames <- f
		}
	}()
	t.Cleanup(func() { c.Close(); b.Close() })
	return c, p
}

func (p *rawPeer) send(typ uint64, flags byte, payload []byte) {
	p.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	p.conn.Write(frame.AppendFrame(nil, frame.Frame{Type: typ, Flags: flags, Payload: payload}))
}

func (p *rawPeer) data(id uint64, flags byte, data []byte) {
	p.send(frame.TypeMuxStream, 0, frame.MuxStream{StreamID: id, Flags: flags, Data: data}.Append(nil))
}

func waitClosed(t testing.TB, c *Conn) error {
	t.Helper()
	select {
	case <-c.Done():
		return c.Err()
	case <-time.After(5 * time.Second):
		t.Fatal("connection not closed")
		return nil
	}
}

func violationOf(t testing.TB, c *Conn) *ViolationError {
	t.Helper()
	var v *ViolationError
	if err := waitClosed(t, c); !errors.As(err, &v) {
		t.Fatalf("closed with %v, want a violation", err)
	}
	return v
}

func TestConnFlowControlViolation(t *testing.T) {
	var abused bool
	c, p := newRawPeer(t, ConnConfig{OnViolation: func(v *ViolationError) { abused = v.Abuse }})
	for sent := 0; sent < DefaultStreamWindow; sent += frame.MaxMuxData {
		p.data(0, 0, make([]byte, frame.MaxMuxData))
	}
	if c.Err() != nil {
		t.Fatalf("data within the window closed the connection: %v", c.Err())
	}
	p.data(0, 0, []byte{0})
	if v := violationOf(t, c); !errors.Is(v, errFlowControl) || !abused {
		t.Fatalf("got %v abuse=%v", v, abused)
	}
}

func TestConnMaxStreamsRefused(t *testing.T) {
	c, p := newRawPeer(t, ConnConfig{MaxStreams: 4})
	for i := uint64(0); i < 5; i++ {
		p.data(2*i, 0, []byte("x"))
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.Type != frame.TypeMuxReset {
				continue
			}
			m, _ := frame.ParseMuxReset(f.Payload)
			if m.StreamID != 8 || m.Code != frame.CodeRefused {
				t.Fatalf("reset %+v, want stream 8 refused", m)
			}
			if c.Err() != nil || c.Stats().StreamsRefused != 1 {
				t.Fatalf("err %v stats %+v", c.Err(), c.Stats())
			}
			// Four streams were accepted.
			for i := 0; i < 4; i++ {
				if _, err := c.AcceptStream(ctxT(t)); err != nil {
					t.Fatal(err)
				}
			}
			return
		case <-deadline:
			t.Fatal("no MUX_RESET for the fifth stream")
		}
	}
}

func TestConnDataTooLargeForMuxFrame(t *testing.T) {
	c, p := newRawPeer(t, ConnConfig{})
	p.data(0, 0, make([]byte, frame.MaxMuxData+1)) // frame ≤ 16400, data > 16384
	if v := violationOf(t, c); !v.Abuse || !frame.IsMalformed(v.Err) {
		t.Fatalf("%v", v)
	}
}

func TestConnFrameOverTypeMax(t *testing.T) {
	c, p := newRawPeer(t, ConnConfig{})
	p.conn.Write(frame.AppendFrameHeader(nil, frame.TypeMuxWindow, 0, 17))
	if v := violationOf(t, c); !frame.IsTooLarge(v.Err) {
		t.Fatalf("%v", v)
	}
}

func TestConnUnknownFrames(t *testing.T) {
	// Unknown with IGNORABLE: skipped, the connection works.
	c, p := newRawPeer(t, ConnConfig{})
	p.send(0x3e, frame.FlagIgnorable, []byte("grease"))
	p.send(0x2f, frame.FlagIgnorable, make([]byte, 3000))
	p.send(frame.TypePadding, frame.FlagIgnorable, make([]byte, 100))
	p.data(0, frame.MuxFlagFin, []byte("after"))
	st, err := c.AcceptStream(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(st); string(b) != "after" {
		t.Fatalf("%q", b)
	}
	if c.Stats().FramesSkipped != 2 {
		t.Fatalf("skipped %d", c.Stats().FramesSkipped)
	}
	// Unknown without IGNORABLE: the connection closes, but it is not abuse.
	p.send(0x2f, 0, []byte("x"))
	if v := violationOf(t, c); v.Abuse {
		t.Fatalf("unknown frame counted as abuse: %v", v)
	}
	// A known session-level type that is not valid at connection level is abuse.
	c2, p2 := newRawPeer(t, ConnConfig{})
	p2.send(frame.TypePing, 0, make([]byte, 8))
	if v := violationOf(t, c2); !v.Abuse || !errors.Is(v, errWrongLevel) {
		t.Fatalf("%v", v)
	}
}

func TestConnStreamRules(t *testing.T) {
	// Data on a local-parity stream that was never opened.
	c, p := newRawPeer(t, ConnConfig{Client: true})
	p.data(6, 0, []byte("x"))
	if v := violationOf(t, c); !errors.Is(v, errUnknownLocalStrm) {
		t.Fatalf("%v", v)
	}
	// Data after FIN.
	c2, p2 := newRawPeer(t, ConnConfig{})
	p2.data(0, frame.MuxFlagFin, []byte("a"))
	p2.data(0, 0, []byte("b"))
	if v := violationOf(t, c2); !errors.Is(v, errDataAfterFin) {
		t.Fatalf("%v", v)
	}
}

func TestConnCloseFailsStreams(t *testing.T) {
	c, s := connPair(t, ConnConfig{}, ConnConfig{})
	ctx := ctxT(t)
	st, _ := c.OpenStream(ctx)
	st.Write([]byte("x"))
	rs, _ := s.AcceptStream(ctx)
	done := make(chan error, 1)
	go func() {
		b := make([]byte, 10)
		rs.Read(b)
		_, err := rs.Read(b)
		done <- err
	}()
	c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read on a dead connection returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked read not woken by the carrier closing")
	}
	if _, err := c.OpenStream(ctx); err == nil {
		t.Fatal("open on a closed connection")
	}
}

func TestConnReadDeadline(t *testing.T) {
	c, _ := connPair(t, ConnConfig{}, ConnConfig{})
	st, _ := c.OpenStream(ctxT(t))
	st.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	start := time.Now()
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, ErrDeadline) {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("deadline not honoured")
	}
}
