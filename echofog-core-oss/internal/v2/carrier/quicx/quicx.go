package quicx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/apernet/quic-go"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

const (
	// ChannelBindingLabel is the TLS exporter label of handshake.md §3.2.
	ChannelBindingLabel = "EXPORTER-echofog/hs2/channel"
	// channelBindingLen is the exporter length of handshake.md §3.2.
	channelBindingLen = 32

	// datagramQueue is how many outgoing datagrams are buffered before new ones
	// are dropped. Carrier datagrams are best effort (mux.Carrier).
	datagramQueue = 64
)

// Application error codes used when the carrier closes a connection itself
// (registries.md §10.2).
const (
	codeNoError           quic.ApplicationErrorCode = 0
	codeProtocolViolation quic.ApplicationErrorCode = 1
	codeInternal          quic.ApplicationErrorCode = 10
)

// ErrClassicalGroup reports that a path that must use X25519MLKEM768 negotiated a classical
// group. The carrier has already closed the connection on both ends when it is returned.
var ErrClassicalGroup = errors.New("quicx: classical TLS group on a path that must be post-quantum")

// Wrap turns one established QUIC connection into a mux.Carrier. client reports which side
// opened the connection; it is true for the dialer and false for the acceptor.
//
// The carrier reports sealed mode from the negotiated TLS group (wire.md §8.5): a classical
// group means sealed, X25519MLKEM768 does not. This is the reality2-quic policy, where the
// group mirrors the cover; noise3 must not allow a classical group at all and uses
// WrapNoise3.
//
// Wrap is called after the QUIC handshake has completed. If the TLS exporter for the channel
// binding fails, the connection is closed and the carrier's operations return that error.
func Wrap(conn *quic.Conn, client bool) mux.Carrier {
	return wrap(conn, client, false)
}

// WrapNoise3 is Wrap with the noise3 policy (wire.md §5.1): the inner TLS must have negotiated
// X25519MLKEM768. A classical group is a protocol failure, so the connection is closed with
// CLOSE{protocol-violation} on both ends and every carrier operation returns
// ErrClassicalGroup.
func WrapNoise3(conn *quic.Conn, client bool) mux.Carrier {
	return wrap(conn, client, true)
}

func wrap(conn *quic.Conn, client, requirePostQuantum bool) *Carrier {
	c := &Carrier{conn: conn, client: client}
	c.lastReceive.Store(time.Now().UnixNano())

	group := conn.ConnectionState().TLS.CurveID
	c.sealed = !isPostQuantum(group)
	if requirePostQuantum && c.sealed {
		_ = conn.CloseWithError(codeProtocolViolation, "classical TLS group")
		c.fail = ErrClassicalGroup
		return c
	}

	binding, err := conn.ExportKeyingMaterial(ChannelBindingLabel, nil, channelBindingLen)
	if err != nil {
		_ = conn.CloseWithError(codeInternal, "channel binding")
		c.fail = fmt.Errorf("quicx: channel binding: %w", err)
		return c
	}
	copy(c.binding[:], binding)

	c.out = make(chan []byte, datagramQueue)
	go c.sendDatagrams()
	go c.refuseUniStreams()
	return c
}

// isPostQuantum reports whether group gives post-quantum confidentiality (wire.md §5.1,
// handshake.md §3.2). Every other group, including an unknown zero, is classical.
func isPostQuantum(group tls.CurveID) bool {
	return group == tls.X25519MLKEM768
}

// Carrier is one QUIC connection seen as a mux.Carrier: native streams, DATAGRAM frames, the
// TLS channel binding and sealed mode.
type Carrier struct {
	conn    *quic.Conn
	client  bool
	binding [32]byte
	sealed  bool

	// fail is set once, before Wrap returns, when the connection cannot be used.
	fail error

	out chan []byte

	lastReceive atomic.Int64 // Unix nanoseconds, updated on every successful read

	closeOnce sync.Once
	closeErr  error
}

var _ mux.Carrier = (*Carrier)(nil)

func (c *Carrier) failure() error { return c.fail }

func (c *Carrier) OpenStream(ctx context.Context) (mux.RawStream, error) {
	if err := c.failure(); err != nil {
		return nil, err
	}
	s, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &stream{Stream: s, carrier: c}, nil
}

func (c *Carrier) AcceptStream(ctx context.Context) (mux.RawStream, error) {
	if err := c.failure(); err != nil {
		return nil, err
	}
	s, err := c.conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &stream{Stream: s, carrier: c}, nil
}

// SendDatagram queues one datagram. It never blocks: when the queue is full or the connection
// is gone, the datagram is dropped, as mux.Carrier requires.
func (c *Carrier) SendDatagram(d []byte) error {
	if err := c.failure(); err != nil {
		return err
	}
	p := make([]byte, len(d))
	copy(p, d)
	select {
	case c.out <- p:
		return nil
	default:
		return nil // best effort
	}
}

func (c *Carrier) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	if err := c.failure(); err != nil {
		return nil, err
	}
	d, err := c.conn.ReceiveDatagram(ctx)
	if err != nil {
		return nil, err
	}
	c.touch()
	return d, nil
}

func (c *Carrier) ChannelBinding() [32]byte { return c.binding }
func (c *Carrier) Sealed() bool             { return c.sealed }
func (c *Carrier) IsClient() bool           { return c.client }

func (c *Carrier) LastReceive() time.Time {
	return time.Unix(0, c.lastReceive.Load())
}

func (c *Carrier) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.conn.CloseWithError(codeNoError, "")
	})
	return c.closeErr
}

func (c *Carrier) Done() <-chan struct{} { return c.conn.Context().Done() }

func (c *Carrier) touch() { c.lastReceive.Store(time.Now().UnixNano()) }

// sendDatagrams drains the best-effort send queue. quic-go's own send queue can block once it
// is full, which is fine: the connection's close ends the loop because SendDatagram then
// returns an error.
func (c *Carrier) sendDatagrams() {
	for {
		select {
		case p := <-c.out:
			_ = c.conn.SendDatagram(p)
		case <-c.conn.Context().Done():
			return
		}
	}
}

// refuseUniStreams enforces the mux.Carrier contract: v2 has no unidirectional streams. The
// noise3 config advertises none at all, but a Chrome-parroting reality2 client must advertise
// Chrome's 103 to keep its fingerprint. Anything a peer opens anyway is cancelled at once.
func (c *Carrier) refuseUniStreams() {
	for {
		s, err := c.conn.AcceptUniStream(c.conn.Context())
		if err != nil {
			return
		}
		s.CancelRead(quic.StreamErrorCode(codeProtocolViolation))
	}
}

// stream adapts one QUIC bidirectional stream to mux.RawStream.
type stream struct {
	*quic.Stream
	carrier *Carrier
}

var _ mux.RawStream = (*stream)(nil)

func (s *stream) ID() uint64 { return uint64(s.Stream.StreamID()) }

func (s *stream) Read(p []byte) (int, error) {
	n, err := s.Stream.Read(p)
	if n > 0 {
		s.carrier.touch()
	}
	return n, resetError(err)
}

func (s *stream) CloseWrite() error { return s.Stream.Close() }

// Close ends the sending direction and abandons the receiving one (mux.RawStream).
func (s *stream) Close() error {
	err := s.Stream.Close()
	s.Stream.CancelRead(quic.StreamErrorCode(codeNoError))
	return err
}

// Reset aborts both directions with the application error code, which the peer reads as a
// remote reset.
func (s *stream) Reset(code uint64) {
	streamCode := quic.StreamErrorCode(code)
	s.Stream.CancelWrite(streamCode)
	s.Stream.CancelRead(streamCode)
}

// resetError translates a QUIC stream error into the mux reset error that the byte-stream mux
// and carriertest expect.
func resetError(err error) error {
	if err == nil {
		return nil
	}
	var streamErr *quic.StreamError
	if errors.As(err, &streamErr) {
		return &mux.ResetError{Code: uint64(streamErr.ErrorCode), Remote: streamErr.Remote}
	}
	return err
}
