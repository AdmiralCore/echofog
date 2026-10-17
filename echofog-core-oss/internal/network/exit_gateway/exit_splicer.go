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
