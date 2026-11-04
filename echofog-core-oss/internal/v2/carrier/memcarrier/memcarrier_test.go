package memcarrier

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/carriertest"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

func TestQUICCarrier(t *testing.T) {
	carriertest.Run(t, func(*testing.T) (mux.Carrier, mux.Carrier) {
		p := NewQUIC()
		return p.Client, p.Server
	})
}

func TestByteCarrier(t *testing.T) {
	carriertest.Run(t, func(*testing.T) (mux.Carrier, mux.Carrier) {
		p := NewBytes()
		return p.Client, p.Server
	})
}

func TestOptions(t *testing.T) {
	cb := [32]byte{1, 2, 3}
	p := NewQUIC(WithChannelBinding(cb), WithSealed(true))
	if p.Client.ChannelBinding() != cb || p.Server.ChannelBinding() != cb {
		t.Fatal("WithChannelBinding was not applied to both sides")
	}
	if !p.Client.Sealed() || !p.Server.Sealed() {
		t.Fatal("WithSealed was not applied to both sides")
	}
	p.Close()

	p = NewQUIC()
	if p.Client.ChannelBinding() != p.Server.ChannelBinding() {
		t.Fatal("sides disagree on channel binding")
	}
	first := p.Client.ChannelBinding()
	p.Close()
	q := NewQUIC()
	if q.Client.ChannelBinding() == first {
		t.Fatal("two pairs got the same random channel binding")
	}
	q.Close()
}

func TestDropClosesBothSides(t *testing.T) {
	for _, mode := range []Mode{QUIC, Byte} {
		p := New(mode)
		p.Drop()
		for _, c := range []mux.Carrier{p.Client, p.Server} {
			select {
			case <-c.Done():
			case <-time.After(2 * time.Second):
				t.Fatalf("mode %d: Done did not close after Drop", mode)
			}
		}
	}
}

func TestLossDropsDatagrams(t *testing.T) {
	p := NewQUIC(WithLoss(1))
	defer p.Close()
	if err := p.Client.SendDatagram([]byte("lost")); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.Server.ReceiveDatagram(ctx); err == nil {
		t.Fatal("a datagram survived loss=1")
	}
}

func TestSetLossAndLatency(t *testing.T) {
	p := NewQUIC()
	defer p.Close()
	p.SetLoss(0)
	p.SetLatency(10 * time.Millisecond)
	p.SetLatency(0)
	if err := p.Client.SendDatagram([]byte("kept")); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := p.Server.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if !bytes.Equal(got, []byte("kept")) {
		t.Fatalf("datagram = %q", got)
	}
}

func TestNativeStreamsStayReliableUnderDatagramLoss(t *testing.T) {
	p := NewQUIC(WithLoss(1))
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := p.Client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := s.Write([]byte("stream")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	rs, err := p.Server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if err := rs.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 16)
	n, err := rs.Read(buf)
	if err != nil || string(buf[:n]) != "stream" {
		t.Fatalf("Read = (%q, %v), want stream", buf[:n], err)
	}
}

func TestDatagramLimits(t *testing.T) {
	p := NewQUIC()
	defer p.Close()
	tooBig := make([]byte, mux.MaxDatagram+1)
	if err := p.Client.SendDatagram(tooBig); !errors.Is(err, mux.ErrDatagramTooLarge) {
		t.Fatalf("SendDatagram(too big) = %v, want ErrDatagramTooLarge", err)
	}
}

func TestLastReceive(t *testing.T) {
	p := NewQUIC()
	defer p.Close()
	if !p.Client.LastReceive().IsZero() {
		t.Fatal("client.LastReceive() is non-zero before any traffic")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Client.SendDatagram([]byte("x")); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	if _, err := p.Server.ReceiveDatagram(ctx); err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if p.Server.LastReceive().IsZero() {
		t.Fatal("server.LastReceive() is zero after receiving")
	}
}

func TestNativeStreamIDs(t *testing.T) {
	p := NewQUIC()
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cs, err := p.Client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("client.OpenStream: %v", err)
	}
	ss, err := p.Server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("server.AcceptStream: %v", err)
	}
	if cs.ID() != 0 {
		t.Fatalf("first client stream ID = %d, want 0", cs.ID())
	}
	if ss.ID() != 0 {
		t.Fatalf("server sees first client stream ID = %d, want 0", ss.ID())
	}
	cs.Close()
	ss.Close()
}

// Make sure the generic carrier suite is clearly tied to the task's suite numbering.
func TestSuiteIDs(t *testing.T) {
	if objects.SuiteNoise3 != 1 || objects.SuiteReality2QUIC != 2 ||
		objects.SuiteReality2TCP != 3 || objects.SuiteWS2 != 4 {
		t.Fatal("registries.md §6.1 suite IDs changed")
	}
}
