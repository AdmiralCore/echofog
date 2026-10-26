package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/apernet/quic-go"
)

// MuxStream is a reliable bidirectional stream inside a peer connection.
type MuxStream interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	// CancelRead tells the remote to stop sending on this stream.
	CancelRead()
	// CloseWrite ends our sending side only: the remote reads EOF and can still answer, like
	// TCP's half-close. Close ends both sides.
	CloseWrite() error
}

// MuxSendStream is the sending half of a one-way stream.
type MuxSendStream interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

// MuxRecvStream is the receiving half of a one-way stream.
type MuxRecvStream interface {
	io.Reader
	SetReadDeadline(time.Time) error
	CancelRead()
}

// MuxConn is an authenticated-able, multiplexed connection to one peer. QUIC provides it
// natively; over TCP it is TLS (browser mimicry) + smux, for networks that block UDP.
type MuxConn interface {
	OpenStreamSync(ctx context.Context) (MuxStream, error)
	AcceptStream(ctx context.Context) (MuxStream, error)
	OpenUniStreamSync(ctx context.Context) (MuxSendStream, error)
	AcceptUniStream(ctx context.Context) (MuxRecvStream, error)
	// SendDatagram is unreliable and never blocks for long: frames may be dropped.
	SendDatagram(b []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)

	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	CloseWithError(code QuicErrorCode, reason string) error
	Context() context.Context
	// Protocol is "quic" or "tcp".
	Protocol() string
	// ChannelBinding is secret keying material unique to this connection (TLS exporter). The
	// peer handshake signs it, so a relay in the middle cannot reuse the signatures.
	ChannelBinding() ([]byte, error)
}

const (
	ProtocolQUIC = "quic"
	ProtocolTCP  = "tcp"
	// ws is not a Protocol (a WebSocket link reports "tcp"): LinkKind tells it apart for the logs
	linkWS = "ws"

	channelBindingLabel = "EXPORTER-echofog-peer-handshake"
	channelBindingLen   = 32
)

// LinkKind names what carries c, for the logs: "quic" (UDP), "tcp" (TLS over TCP) or "ws"
// (WebSocket, possibly through a CDN).
func LinkKind(c MuxConn) string {
	if s, ok := c.(*smuxConn); ok && s.ws {
		return linkWS
	}
	return c.Protocol()
}

// DescribeLink is LinkKind with both ends, e.g. "quic [::]:60585->203.0.113.7:443": which socket
// of ours talks to which address.
func DescribeLink(c MuxConn) string {
	if c == nil {
		return ""
	}
	local, remote := "?", "?"
	if a := c.LocalAddr(); a != nil {
		local = a.String()
	}
	if a := c.RemoteAddr(); a != nil {
		remote = a.String()
	}
	return LinkKind(c) + " " + local + "->" + remote
}

// quicMuxConn adapts *quic.Conn.
type quicMuxConn struct{ c *quic.Conn }

// WrapQUIC exposes a QUIC connection as a MuxConn.
func WrapQUIC(c *quic.Conn) MuxConn { return quicMuxConn{c} }

type quicStream struct{ *quic.Stream }

func (s quicStream) CancelRead() { s.Stream.CancelRead(quic.StreamErrorCode(ErrCodeNormalClose)) }

// CloseWrite: a QUIC stream's Close already closes only the sending direction.
func (s quicStream) CloseWrite() error { return s.Stream.Close() }

type quicRecvStream struct{ *quic.ReceiveStream }

func (s quicRecvStream) CancelRead() {
	s.ReceiveStream.CancelRead(quic.StreamErrorCode(ErrCodeStreamError))
}

func (q quicMuxConn) OpenStreamSync(ctx context.Context) (MuxStream, error) {
	s, err := q.c.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return quicStream{s}, nil
}

func (q quicMuxConn) AcceptStream(ctx context.Context) (MuxStream, error) {
	s, err := q.c.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return quicStream{s}, nil
}

func (q quicMuxConn) OpenUniStreamSync(ctx context.Context) (MuxSendStream, error) {
	return q.c.OpenUniStreamSync(ctx)
}

func (q quicMuxConn) AcceptUniStream(ctx context.Context) (MuxRecvStream, error) {
	s, err := q.c.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return quicRecvStream{s}, nil
}

func (q quicMuxConn) SendDatagram(b []byte) error {
	err := q.c.SendDatagram(b)
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		return &DatagramTooLargeError{Size: len(b), Max: int(tooLarge.MaxDatagramPayloadSize)}
	}
	return err
}

func (q quicMuxConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return q.c.ReceiveDatagram(ctx)
}

func (q quicMuxConn) LocalAddr() net.Addr      { return q.c.LocalAddr() }
func (q quicMuxConn) RemoteAddr() net.Addr     { return q.c.RemoteAddr() }
func (q quicMuxConn) Context() context.Context { return q.c.Context() }
func (q quicMuxConn) Protocol() string         { return ProtocolQUIC }

func (q quicMuxConn) CloseWithError(code QuicErrorCode, reason string) error {
	return q.c.CloseWithError(code, reason)
}

func (q quicMuxConn) ChannelBinding() (res []byte, err error) {
	select {
	case <-q.c.HandshakeComplete():
	case <-q.c.Context().Done():
		return nil, q.c.Context().Err()
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tls export keying material not available: %v", r)
		}
	}()
	// Conn.ExportKeyingMaterial, not ConnectionState().TLS.ExportKeyingMaterial: the REALITY dial
	// parrots Chrome via uTLS (patched apernet/quic-go fork), whose reconstructed tls.ConnectionState
	// cannot carry the exporter. The fork's method routes to the right exporter for either path.
	return q.c.ExportKeyingMaterial(channelBindingLabel, nil, channelBindingLen)
}

// DatagramTooLargeError: the frame does not fit into one datagram of this connection.
type DatagramTooLargeError struct{ Size, Max int }

func (e *DatagramTooLargeError) Error() string {
	return fmt.Sprintf("%s: %d > %d", ErrDatagramTooLarge, e.Size, e.Max)
}
func (e *DatagramTooLargeError) Unwrap() error { return ErrDatagramTooLarge }
