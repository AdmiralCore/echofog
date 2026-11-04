// Package mux carries an EchoFog v2 session (docs/protocol/v2/wire.md §5.2, §6–§8): the native
// byte-stream mux that replaces smux (Decision D14), the session-level stream layer (OPEN,
// CLOSE, UNSUPPORTED delivery), the control stream (PING, PONG, GOAWAY, PATH_ABANDON), paths and
// PATH_BIND, sealed records and sealed mode, and datagrams.
//
// The package does not run the handshake. It consumes the result of one through the small Keys
// interface (session ID, role, session secret, final hash, transport keys), so it can be tested
// with fixed keys and wired to any handshake implementation.
package mux

import (
	"context"
	"io"
	"time"
)

// RawStream is one stream of a carrier: a QUIC stream, or a stream of the byte-stream mux.
type RawStream interface {
	io.Reader
	io.Writer
	// ID is the carrier's stream ID; 0 is the first stream of the carrier client.
	ID() uint64
	// CloseWrite ends the sending direction (FIN).
	CloseWrite() error
	// Close ends the sending direction and abandons the receiving one.
	Close() error
	// Reset aborts the stream in both directions with an application error code.
	Reset(code uint64)
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// Carrier is one carrier connection: QUIC (native streams and DATAGRAM frames) or a byte-stream
// carrier under the mux of this package (TCP suites, ws2, relay stream circuits).
type Carrier interface {
	OpenStream(ctx context.Context) (RawStream, error)
	AcceptStream(ctx context.Context) (RawStream, error)
	// SendDatagram sends one datagram (wire.md §6.5) best effort; a full queue drops it.
	SendDatagram(d []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	// ChannelBinding is cb of the carrier's TLS session (handshake.md §3.2).
	ChannelBinding() [32]byte
	// Sealed reports that the carrier's TLS negotiated a classical group, so the path runs in
	// sealed mode (wire.md §8.5). Both sides know this from the negotiated group.
	Sealed() bool
	// IsClient reports whether this side opened the carrier (its streams have even IDs).
	IsClient() bool
	// LastReceive is the time the carrier last received authenticated bytes.
	LastReceive() time.Time
	Close() error
	Done() <-chan struct{}
}
