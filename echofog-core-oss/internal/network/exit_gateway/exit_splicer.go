package exit_gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

// ExitSplicer handles the egress boundary: splicing P2P QUIC streams into outbound TCP connections.
type ExitSplicer struct {
	logger      *slog.Logger
	dialTimeout time.Duration

	totalServedStreams atomic.Uint64
	bytesRelayed       atomic.Uint64
}

// NewExitSplicer creates an exit splicer instance.
func NewExitSplicer(dialTimeout time.Duration) *ExitSplicer {
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	return &ExitSplicer{
		logger:      slog.Default().With("component", "exit_splicer"),
		dialTimeout: dialTimeout,
	}
}

// HandleExitStream bridges a P2P stream to an external Internet target (QUIC-to-TCP Splicing).
func (s *ExitSplicer) HandleExitStream(ctx context.Context, p2pStream io.ReadWriteCloser, targetAddr string) error {
	s.totalServedStreams.Add(1)

	// 1. Dial the external Internet target server
	dialer := &net.Dialer{Timeout: s.dialTimeout}
	tcpConn, err := dialer.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		s.logger.Warn("Exit node failed to dial target", "target", targetAddr, "err", err)
		return fmt.Errorf("dial external target %s: %w", targetAddr, err)
	}
	defer tcpConn.Close()

	return s.Splice(ctx, p2pStream, tcpConn)
}

// Splice copies bytes both ways between an established P2P stream and an outbound connection until either side ends.
func (s *ExitSplicer) Splice(ctx context.Context, p2pStream io.ReadWriteCloser, tcpConn net.Conn) error {
	s.logger.Debug("Exit splicing started", "target", tcpConn.RemoteAddr().String())

	// both directions until both are done: a client's half-close reaches the target as one
	count := func(n int) { s.bytesRelayed.Add(uint64(n)) }
	return relay(ctx, p2pStream, tcpConn, count, count)
}

// SpliceFor is Splice that ends the pair once it moved nothing either way for idle, or once it
// lived for life (0 turns either off). Both sides are closed, as at any other end.
func (s *ExitSplicer) SpliceFor(ctx context.Context, p2pStream io.ReadWriteCloser, tcpConn net.Conn, idle, life time.Duration) error {
	if idle <= 0 && life <= 0 {
		return s.Splice(ctx, p2pStream, tcpConn)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := time.Now()
	var last atomic.Int64
	last.Store(start.UnixNano())
	count := func(n int) {
		s.bytesRelayed.Add(uint64(n))
		last.Store(time.Now().UnixNano())
	}
	every := 30 * time.Second
	for _, d := range []time.Duration{idle, life} {
		if d > 0 {
			every = min(every, max(d/4, 10*time.Millisecond))
		}
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if (idle > 0 && now.Sub(time.Unix(0, last.Load())) > idle) || (life > 0 && now.Sub(start) > life) {
					cancel()
					return
				}
			}
		}
	}()
	return relay(ctx, p2pStream, tcpConn, count, count)
}

// Stats returns the cumulative metrics of this Exit Node.
func (s *ExitSplicer) Stats() (servedStreams uint64, totalBytes uint64) {
	return s.totalServedStreams.Load(), s.bytesRelayed.Load()
}

type countingWriter struct {
	w io.Writer
	n *atomic.Uint64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(uint64(n))
	return n, err
}
