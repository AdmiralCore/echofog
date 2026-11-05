package reality2

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// maxRecordPlaintext is the most application data one TLS record carries (RFC 8446 §5.1).
const maxRecordPlaintext = 1 << 14

// tlsSide is what the carrier needs of the finished TLS session of either stack.
type tlsSide interface {
	net.Conn
}

// newTCPCarrier is the byte-stream carrier of reality2-tcp (wire.md §4.3, §5.2): the mux over the
// TLS application data. sealed is decided by the negotiated group (wire.md §8.5): a classical
// group makes the path sealed, and then padding stops being the record layer's job (below).
func newTCPCarrier(c tlsSide, client bool, cb [32]byte, sealed bool, prof *shape.Profile) *mux.Conn {
	return mux.NewConn(wrapPad(c, sealed, prof), mux.ConnConfig{Client: client, ChannelBinding: cb, Sealed: sealed})
}

// wrapPad is the writer the mux of a carrier writes to: the TLS connection itself, or a padding
// writer over it when the profile pads and the path is not sealed.
func wrapPad(c net.Conn, sealed bool, prof *shape.Profile) io.ReadWriteCloser {
	if !sealed && prof != nil && (prof.Pad != nil || len(prof.Buckets) > 0) {
		return &padConn{Conn: c, sender: shape.NewSender(prof, nil, false)}
	}
	return c
}

// padConn adds PADDING frames (wire.md §6.4) to what the mux writes, so that TLS records follow
// the shape profile (wire.md §4.3, §9): during the profile's padding budget each write, which the
// mux makes of whole frames and which fits one record, is rounded up by PadFor. It is the only
// padding mechanism on TCP carriers (V6).
//
// Padding is not applied on a sealed path: there the mux turns the stream into sealed records
// (wire.md §8.5) at a point the byte layer cannot see, and a plaintext PADDING frame after that
// point would end the path. The records of a sealed path carry their own padding (§8.4).
//
// padConn is used by the mux's single writer goroutine, so the sender needs no lock.
type padConn struct {
	net.Conn
	sender *shape.Sender
}

// Write pads p when it fits one record and the budget allows; larger writes pass through (the TLS
// layer splits them) and only spend the budget.
func (p *padConn) Write(b []byte) (int, error) {
	pad := p.sender.PadFor(len(b))
	if pad > maxRecordPlaintext-len(b) {
		pad = maxRecordPlaintext - len(b)
	}
	if pad <= 0 || len(b) > maxRecordPlaintext {
		return p.Conn.Write(b)
	}
	frm, ok := paddingFrame(pad)
	if !ok {
		return p.Conn.Write(b)
	}
	out := make([]byte, 0, len(b)+len(frm))
	out = append(out, b...)
	out = append(out, frm...)
	if _, err := p.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// paddingFrame returns a PADDING frame of exactly n bytes, or false when n is too small for the
// frame header (type, flags, length: 3 bytes).
func paddingFrame(n int) ([]byte, bool) {
	const minFrame = 3
	if n < minFrame {
		return nil, false
	}
	payload := n - minFrame
	if payload >= 64 { // the length no longer fits one varint byte
		payload = n - (minFrame + 1)
	}
	b := frame.AppendFrameHeader(make([]byte, 0, n), frame.TypePadding, 0, uint64(payload))
	b = append(b, make([]byte, payload)...)
	if len(b) != n {
		return nil, false
	}
	return b, true
}

// tlsFacts reads the channel binding of a finished TLS session.
func channelBinding(export func(label string, ctx []byte, n int) ([]byte, error)) ([32]byte, error) {
	var cb [32]byte
	b, err := export(quicx.ChannelBindingLabel, nil, len(cb))
	if err != nil {
		return cb, fmt.Errorf("reality2: channel binding: %w", err)
	}
	copy(cb[:], b)
	return cb, nil
}

// isPostQuantumGroup mirrors quicx: only X25519MLKEM768 gives post-quantum confidentiality; every
// other group, an unknown zero included, runs the path in sealed mode (wire.md §5.1).
func isPostQuantumGroup(g uint16) bool { return tls.CurveID(g) == tls.X25519MLKEM768 }

// settle bounds a blocking call on a connection by a deadline and clears it afterwards.
func settle(c net.Conn, d time.Duration) func() {
	_ = c.SetDeadline(time.Now().Add(d))
	return func() { _ = c.SetDeadline(time.Time{}) }
}
