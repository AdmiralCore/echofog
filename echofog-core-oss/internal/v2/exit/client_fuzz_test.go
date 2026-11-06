package exit

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// FuzzClientHandleDatagram: no datagram from the exit can panic the client, and whatever reaches
// the tunnel is a well-formed v1 frame of one whole packet.
func FuzzClientHandleDatagram(f *testing.F) {
	f.Add(Fragment{Session: 7, Packet: 1, Count: 1, Addr: "192.0.2.1:53", Data: []byte("x")}.Append(nil))
	f.Add(Fragment{Session: 7, Packet: 2, Index: 1, Count: 2, Data: []byte("tail")}.Append(nil))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := NewClient(ClientConfig{
			OpenStream:   func(context.Context) (*mux.Stream, error) { return nil, io.EOF },
			SendDatagram: func(uint64, []byte) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		var got [][]byte
		if c.subscribeForTest(7, func(p []byte) { got = append(got, append([]byte(nil), p...)) }) == nil {
			t.Fatal("subscribe failed")
		}
		c.HandleDatagram(b)
		for _, p := range got {
			if len(p) < 6 {
				t.Fatalf("short v1 frame %x", p)
			}
			n := int(binary.BigEndian.Uint16(p[4:]))
			if p[2] != 0 || p[3] != 1 || n == 0 || len(p) < 6+n {
				t.Fatalf("malformed v1 frame %x", p)
			}
		}
	})
}

// FuzzClientLinkSend: arbitrary v1 frames from the core are either refused or become one
// well-formed EXIT_UDP datagram that fits MUX_DATAGRAM.
func FuzzClientLinkSend(f *testing.F) {
	f.Add(append([]byte{0, 0, 0, 7, 0, 1, 0, 1, 0, 3}, append([]byte("a:1"), 'p')...))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		var sent [][]byte
		c, err := NewClient(ClientConfig{
			OpenStream:   func(context.Context) (*mux.Stream, error) { return nil, io.EOF },
			SendDatagram: func(_ uint64, p []byte) error { sent = append(sent, append([]byte(nil), p...)); return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := (clientLink{c: c}).Send(b); err != nil {
			if len(sent) != 0 {
				t.Fatal("refused but sent")
			}
			return
		}
		if len(sent) != 1 || len(sent[0]) > mux.MaxDatagram-1 {
			t.Fatalf("sent %d datagrams, first %d bytes", len(sent), len(sent[0]))
		}
		if _, err := ParseFragment(sent[0]); err != nil {
			t.Fatalf("emitted a malformed EXIT_UDP datagram: %v", err)
		}
	})
}

// FuzzClientStreamRead: whatever frames the exit sends, ReadFrame and Read return errors or
// bounded data, never panic.
func FuzzClientStreamRead(f *testing.F) {
	f.Add(uint8(0), TypeResult, []byte{0x08, 0x02})
	f.Add(uint8(1), TypeUDPOK, []byte{0x08, 0x01, 0x10, 0x05})
	f.Add(uint8(1), TypeUDPPkt, []byte{0x0a, 0x03, 'a', ':', '1', 0x12, 0x01, 'x'})
	f.Add(uint8(0), TypeData, []byte("hello"))
	f.Fuzz(func(t *testing.T, mode uint8, typ uint64, payload []byte) {
		rec := newRecordedStream()
		rec.in <- frame.Frame{Type: typ, Payload: payload}
		cs := newClientStream(rec, nil, func(time.Duration) {}, nil)
		cs.mode = tunnelMode(mode % 3)
		if cs.mode == modeUDP {
			cs.req = 5
		}
		if _, data, err := cs.ReadFrame(); err == nil && len(data) > 70_000 {
			t.Fatalf("%d bytes from one frame", len(data))
		}
		rec.in <- frame.Frame{Type: typ, Payload: payload}
		_, _ = cs.Read(make([]byte, 8))
	})
}

// subscribeForTest registers a receiver without a core session (fuzz).
func (c *Client) subscribeForTest(id uint32, fn func([]byte)) func() {
	return clientLink{c: c}.Subscribe(id, fn)
}

// TestClientFirstPageLatencyReport measures the cost of H1 on a page-sized exchange: a small
// request, a 40 KiB response, over a loopback tunnel with real jitter. It asserts nothing about
// time (it would flake); the numbers go to the report (-v).
func TestClientFirstPageLatencyReport(t *testing.T) {
	if testing.Short() {
		t.Skip("timing report")
	}
	page := bytes.Repeat([]byte("<p>page</p>"), 4000)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 512)
				if _, err := c.Read(buf); err != nil {
					return
				}
				_, _ = c.Write(page)
			}()
		}
	}()

	tls, _ := shape.Builtin(shape.IDDefaultTLS)
	// Two rigs only: many nodes in one Windows test binary exhaust memguard's locked memory.
	plainRig := newClientRigWith(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true}}, nil, func(time.Duration) {})
	paddedRig := newClientRigWith(t, rigConfig{ServerConfig: ServerConfig{AllowPrivateTargets: true, Shape: tls}}, tls, time.Sleep)
	measure := func(cr *clientRig) time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		conn, err := cr.cl.DialExitStream(ctx, ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		readFull(t, conn, len(page))
		return time.Since(start)
	}
	var plain, padded time.Duration
	const runs = 5
	for i := 0; i < runs; i++ {
		plain += measure(plainRig)
		padded += measure(paddedRig)
	}
	t.Logf("first page (40 KiB), mean of %d: plain %v, padded (default-tls both ways, real jitter) %v",
		runs, plain/runs, padded/runs)
}
