package handshake

import (
	"io"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
)

// closingFrames is one end of an in-memory pipe whose writer side is closed when its own
// negotiation ends, so the other side reads EOF instead of waiting for a timeout.
type closingFrames struct {
	in  <-chan []byte
	out chan<- []byte
}

func (c closingFrames) ReadFrame() (net_types.MessageType, []byte, error) {
	select {
	case b, ok := <-c.in:
		if !ok {
			return 0, nil, io.EOF
		}
		return net_types.TypeHandshake, b, nil
	case <-time.After(2 * time.Second):
		return 0, nil, io.ErrUnexpectedEOF
	}
}

func (c closingFrames) WriteFrame(_ net_types.MessageType, d []byte) error {
	c.out <- append([]byte(nil), d...)
	return nil
}

// FuzzVersionsAndCaps runs a real handshake between two honest nodes whose protocol versions,
// capabilities and announced listen ports are chosen by the fuzzer: a node below
// MinProtocolVersion is refused by the other side, and when both accept, each learns exactly what
// the other announced.
func FuzzVersionsAndCaps(f *testing.F) {
	f.Add(uint32(MinProtocolVersion), uint32(MinProtocolVersion), uint32(DefaultCaps), uint32(CapServer), uint16(443))
	f.Add(uint32(MinProtocolVersion-1), uint32(MinProtocolVersion+1), uint32(0), uint32(0xffffffff), uint16(0))
	f.Add(uint32(0xffffffff), uint32(0), uint32(CapPoolLink), uint32(0), uint16(65535))
	f.Fuzz(func(t *testing.T, va, vb, capsA, capsB uint32, port uint16) {
		ab, ba := make(chan []byte, 8), make(chan []byte, 8)
		a := NewNegotiator(newSigner(t), &NegotiatorConfig{MyVersion: va, MyCaps: Capability(capsA), ListenPort: port}, closingFrames{in: ba, out: ab})
		b := NewNegotiator(newSigner(t), &NegotiatorConfig{MyVersion: vb, MyCaps: Capability(capsB)}, closingFrames{in: ab, out: ba})
		type out struct {
			res *HandshakeResult
			err error
		}
		ra, rb := make(chan out, 1), make(chan out, 1)
		go func() { r, err := a.Negotiate(binding(3), true); close(ab); ra <- out{r, err} }()
		go func() { r, err := b.Negotiate(binding(3), false); close(ba); rb <- out{r, err} }()
		ga, gb := <-ra, <-rb
		if va < MinProtocolVersion && gb.err == nil {
			t.Fatalf("responder accepted an initiator of protocol %d", va)
		}
		if vb < MinProtocolVersion && ga.err == nil {
			t.Fatalf("initiator accepted a responder of protocol %d", vb)
		}
		if ga.err != nil || gb.err != nil {
			return
		}
		if ga.res.Version != vb || ga.res.Caps != Capability(capsB) {
			t.Fatalf("initiator learnt v%d caps %x, responder announced v%d caps %x", ga.res.Version, ga.res.Caps, vb, capsB)
		}
		if gb.res.Version != va || gb.res.Caps != Capability(capsA) || gb.res.ListenPort != port {
			t.Fatalf("responder learnt v%d caps %x port %d, initiator announced v%d caps %x port %d",
				gb.res.Version, gb.res.Caps, gb.res.ListenPort, va, capsA, port)
		}
	})
}
