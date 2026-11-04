package udpsock

import (
	"bytes"
	"net"
	"testing"
)

const (
	benchDatagram = 1200
	benchBatch    = 32
)

func benchPair(b *testing.B) (*Conn, *Conn) {
	b.Helper()
	a, err := Listen(b.Context(), "udp4", nil, Options{})
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	peer, err := Listen(b.Context(), "udp4", nil, Options{})
	if err != nil {
		a.Close()
		b.Fatalf("listen: %v", err)
	}
	if err := peer.SetReadBuffer(16 << 20); err != nil {
		b.Fatalf("receive buffer: %v", err)
	}
	b.Cleanup(func() { a.Close(); peer.Close() })

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ms := messages(64, 2048)
		for {
			if _, err := peer.ReadBatch(ms, 0); err != nil {
				return
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	b.Cleanup(func() { close(stop); peer.Close(); <-done })
	return a, peer
}

func BenchmarkWriteBatchGSO(b *testing.B) {
	a, peer := benchPair(b)
	dst := peer.LocalAddr().(*net.UDPAddr)
	bufs := make([][]byte, benchBatch)
	for i := range bufs {
		bufs[i] = bytes.Repeat([]byte{byte(i)}, benchDatagram)
	}
	b.SetBytes(int64(benchDatagram * benchBatch))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n, err := a.WriteBatch(dst, bufs); err != nil || n != len(bufs) {
			b.Fatalf("batch write: %d %v", n, err)
		}
	}
	b.StopTimer()
	if a.gso.Load() {
		b.ReportMetric(1, "gso")
	} else {
		b.ReportMetric(0, "gso")
	}
}

func BenchmarkWriteToEach(b *testing.B) {
	a, peer := benchPair(b)
	dst := peer.LocalAddr().(*net.UDPAddr)
	bufs := make([][]byte, benchBatch)
	for i := range bufs {
		bufs[i] = bytes.Repeat([]byte{byte(i)}, benchDatagram)
	}
	b.SetBytes(int64(benchDatagram * benchBatch))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range bufs {
			if _, err := a.WriteTo(p, dst); err != nil {
				b.Fatalf("write: %v", err)
			}
		}
	}
}
