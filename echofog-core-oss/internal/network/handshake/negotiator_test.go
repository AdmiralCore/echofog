package handshake

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type keySigner struct{ priv ed25519.PrivateKey }

func newSigner(t *testing.T) keySigner {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return keySigner{priv}
}

func (k keySigner) GetPublicKey() ([]byte, error) { return k.priv.Public().(ed25519.PublicKey), nil }
func (k keySigner) SignData(d []byte) ([]byte, error) {
	return ed25519.Sign(k.priv, d), nil
}

// chanFrames is one end of an in-memory frame pipe.
type chanFrames struct {
	in  <-chan []byte
	out chan<- []byte
}

func (c chanFrames) ReadFrame() (net_types.MessageType, []byte, error) {
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

func (c chanFrames) WriteFrame(_ net_types.MessageType, d []byte) error {
	c.out <- append([]byte(nil), d...)
	return nil
}

func pipe() (chanFrames, chanFrames) {
	ab, ba := make(chan []byte, 8), make(chan []byte, 8)
	return chanFrames{in: ba, out: ab}, chanFrames{in: ab, out: ba}
}

func cfg() *NegotiatorConfig { return &NegotiatorConfig{MyVersion: MinProtocolVersion} }

func binding(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

type result struct {
	res *HandshakeResult
	err error
}

func run(n *Negotiator, bind []byte, initiator bool) chan result {
	ch := make(chan result, 1)
	go func() {
		r, err := n.Negotiate(bind, initiator)
		ch <- result{r, err}
	}()
	return ch
}

func TestMutualAuthentication(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	fa, fb := pipe()
	ra := run(NewNegotiator(a, cfg(), fa), binding(1), true)
	rb := run(NewNegotiator(b, cfg(), fb), binding(1), false)
	got1, got2 := <-ra, <-rb
	require.NoError(t, got1.err)
	require.NoError(t, got2.err)

	pa, _ := a.GetPublicKey()
	pb, _ := b.GetPublicKey()
	assert.Equal(t, types.PeerPublicKey(pb), got1.res.PubKey, "initiator learns the responder")
	assert.Equal(t, types.PeerPublicKey(pa), got2.res.PubKey, "responder learns the initiator")
}

// A relay in the middle terminates two TLS sessions; their channel bindings differ, so the
// signatures it forwards do not verify on either side.
func TestRelayInTheMiddleFails(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	fa, fb := pipe()
	ra := run(NewNegotiator(a, cfg(), fa), binding(1), true)
	rb := run(NewNegotiator(b, cfg(), fb), binding(2), false)
	assert.Error(t, (<-ra).err)
	assert.Error(t, (<-rb).err)
}

// The initiator claims somebody else's public key: without that key it cannot sign.
func TestInitiatorCannotImpersonate(t *testing.T) {
	victim, attacker, server := newSigner(t), newSigner(t), newSigner(t)
	fa, fb := pipe()
	rb := run(NewNegotiator(server, cfg(), fb), binding(3), false)

	vpub, _ := victim.GetPublicKey()
	nonce := make([]byte, nonceLen)
	init, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_HandshakeInit{HandshakeInit: &internal_pb.HandshakeInit{
		Nonce: nonce, Version: MinProtocolVersion, NetworkId: make([]byte, 32), PubKey: vpub,
	}}})
	require.NoError(t, fa.WriteFrame(0, init))
	_, _, err := fa.ReadFrame() // the server's response
	require.NoError(t, err)

	spub, _ := server.GetPublicKey()
	sig, _ := attacker.SignData(transcript(labelInitiator, binding(3), nonce, vpub, spub))
	proof, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_HandshakeResponse{HandshakeResponse: &internal_pb.HandshakeResponse{
		PubKey: vpub, Signature: sig,
	}}})
	require.NoError(t, fa.WriteFrame(0, proof))
	res := <-rb
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "prove")
}

func TestMalformedInitDoesNotPanic(t *testing.T) {
	for name, init := range map[string]*internal_pb.HandshakeInit{
		"short network id": {Nonce: make([]byte, 32), Version: MinProtocolVersion, NetworkId: []byte{1}, PubKey: make([]byte, 32)},
		"short pubkey":     {Nonce: make([]byte, 32), Version: MinProtocolVersion, NetworkId: make([]byte, 32), PubKey: []byte{1, 2}},
		"short nonce":      {Nonce: []byte{1}, Version: MinProtocolVersion, NetworkId: make([]byte, 32), PubKey: make([]byte, 32)},
		"old version":      {Nonce: make([]byte, 32), Version: 100, NetworkId: make([]byte, 32), PubKey: make([]byte, 32)},
	} {
		t.Run(name, func(t *testing.T) {
			fa, fb := pipe()
			rb := run(NewNegotiator(newSigner(t), cfg(), fb), binding(4), false)
			b, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_HandshakeInit{HandshakeInit: init}})
			require.NoError(t, fa.WriteFrame(0, b))
			assert.Error(t, (<-rb).err)
		})
	}
}

func TestInitiatorRefusesAbsurdPuzzle(t *testing.T) {
	fa, fb := pipe()
	ra := run(NewNegotiator(newSigner(t), cfg(), fa), binding(5), true)
	_, _, err := fb.ReadFrame()
	require.NoError(t, err)
	b, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_PuzzleChallenge{PuzzleChallenge: &internal_pb.PuzzleChallenge{Prefix: []byte{1}, Difficulty: 200}}})
	require.NoError(t, fb.WriteFrame(0, b))
	select {
	case r := <-ra:
		assert.Error(t, r.err)
	case <-time.After(3 * time.Second):
		t.Fatal("initiator is still grinding an impossible puzzle")
	}
}

// tap records the frames that one end writes.
type tap struct {
	chanFrames
	mu     sync.Mutex
	frames [][]byte
}

func (p *tap) WriteFrame(t net_types.MessageType, d []byte) error {
	p.mu.Lock()
	p.frames = append(p.frames, append([]byte(nil), d...))
	p.mu.Unlock()
	return p.chanFrames.WriteFrame(t, d)
}

func (p *tap) challenge() *internal_pb.PuzzleChallenge {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.frames {
		var m internal_pb.MessageData
		if proto.Unmarshal(f, &m) == nil && m.GetPuzzleChallenge() != nil {
			return m.GetPuzzleChallenge()
		}
	}
	return nil
}

func handshakeWith(t *testing.T, responder *NegotiatorConfig) (challenge *internal_pb.PuzzleChallenge, err error) {
	t.Helper()
	fa, fb := pipe()
	tb := &tap{chanFrames: fb}
	ra := run(NewNegotiator(newSigner(t), cfg(), fa), binding(9), true)
	rb := run(NewNegotiator(newSigner(t), responder, tb), binding(9), false)
	got1, got2 := <-ra, <-rb
	if got1.err != nil {
		return tb.challenge(), got1.err
	}
	return tb.challenge(), got2.err
}

func TestNoPuzzleWhileTheNodeIsCalm(t *testing.T) {
	c := cfg()
	c.AntiDDoS = NewAntiDDoS()
	ch, err := handshakeWith(t, c)
	require.NoError(t, err)
	assert.Nil(t, ch, "a calm node does not make newcomers work")
}

func TestPuzzleIsAskedUnderLoadAndSolved(t *testing.T) {
	c := cfg()
	c.AntiDDoS = NewAntiDDoS()
	for i := 0; i < 200; i++ { // about 66 handshakes per second: a puzzle that is cheap to solve even slowly
		c.AntiDDoS.Note()
	}
	ch, err := handshakeWith(t, c)
	require.NoError(t, err, "an honest peer solves the puzzle and gets in")
	require.NotNil(t, ch)
	assert.GreaterOrEqual(t, ch.Difficulty, uint32(busyBits))
}

func TestPuzzleCanBeRequiredAlways(t *testing.T) {
	c := cfg()
	c.RequirePoW = true
	c.AntiDDoS = NewAntiDDoS()
	ch, err := handshakeWith(t, c)
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.GreaterOrEqual(t, ch.Difficulty, uint32(minPoWBits))
}

func TestWrongPuzzleSolutionIsRefused(t *testing.T) {
	c := cfg()
	c.RequirePoW = true
	fa, fb := pipe()
	rb := run(NewNegotiator(newSigner(t), c, fb), binding(3), false)

	hello, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_HandshakeInit{HandshakeInit: &internal_pb.HandshakeInit{
		PubKey: bytes.Repeat([]byte{1}, 32), Nonce: bytes.Repeat([]byte{2}, nonceLen), Version: MinProtocolVersion, NetworkId: make([]byte, 32),
	}}})
	require.NoError(t, fa.WriteFrame(0, hello))
	_, _, err := fa.ReadFrame() // the challenge
	require.NoError(t, err)
	bad, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_PuzzleSolution{PuzzleSolution: &internal_pb.PuzzleSolution{Nonce: 12345}}})
	require.NoError(t, fa.WriteFrame(0, bad))
	r := <-rb
	assert.Error(t, r.err, "a guess is not a solution")
}
