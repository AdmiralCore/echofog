package handshake

import (
	"testing"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pubID(t *testing.T, s keySigner) types.PeerID {
	pub, _ := s.GetPublicKey()
	var k types.PeerPublicKey
	copy(k[:], pub)
	return types.PeerPubKeyToID(k)
}

// hidden returns a responder config behind gate.
func hidden(gate *access.Gate) *NegotiatorConfig {
	c := cfg()
	c.Admit = gate.AdmitHandshake
	return c
}

func withProof(secret []byte) *NegotiatorConfig {
	c := cfg()
	c.AccessProof = func(binding, nonce, myPub []byte) []byte {
		return access.HandshakeProof(secret, binding, nonce, myPub)
	}
	return c
}

func newHiddenGate(t *testing.T) (*access.Gate, []byte) {
	g := access.NewGate()
	secret := access.NewSecret()
	require.NoError(t, g.SetSecret(secret))
	g.Enable(true)
	return g, secret
}

// A stranger gets nothing from a hidden node: not its key, not a puzzle, not a word.
func TestHiddenNodeSilentToStranger(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	gate, _ := newHiddenGate(t)
	fa, fb := pipe()
	ra := run(NewNegotiator(a, cfg(), fa), binding(1), true)
	rb := run(NewNegotiator(b, hidden(gate), fb), binding(1), false)

	got := <-rb
	assert.ErrorIs(t, got.err, ErrAccessDenied)
	assert.Empty(t, fb.out, "the hidden node must send nothing to a stranger")
	assert.Error(t, (<-ra).err)
	assert.Empty(t, gate.Joined())
}

func TestHiddenNodeWrongSecret(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	gate, _ := newHiddenGate(t)
	fa, fb := pipe()
	ra := run(NewNegotiator(a, withProof(access.NewSecret()), fa), binding(1), true)
	rb := run(NewNegotiator(b, hidden(gate), fb), binding(1), false)
	assert.ErrorIs(t, (<-rb).err, ErrAccessDenied)
	assert.Error(t, (<-ra).err)
}

// The proof is bound to the connection: replayed on another one it is worthless.
func TestHiddenNodeProofBoundToConnection(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	gate, secret := newHiddenGate(t)
	c := cfg()
	c.AccessProof = func(_, nonce, myPub []byte) []byte {
		return access.HandshakeProof(secret, binding(9), nonce, myPub) // another connection's binding
	}
	fa, fb := pipe()
	ra := run(NewNegotiator(a, c, fa), binding(1), true)
	rb := run(NewNegotiator(b, hidden(gate), fb), binding(1), false)
	assert.ErrorIs(t, (<-rb).err, ErrAccessDenied)
	assert.Error(t, (<-ra).err)
}

func TestHiddenNodeAdmitsSecretAndRemembers(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	gate, secret := newHiddenGate(t)

	fa, fb := pipe()
	ra := run(NewNegotiator(a, withProof(secret), fa), binding(1), true)
	rb := run(NewNegotiator(b, hidden(gate), fb), binding(1), false)
	require.NoError(t, (<-ra).err)
	require.NoError(t, (<-rb).err)
	assert.Contains(t, gate.Joined(), pubID(t, a))

	// a new secret: the link given out before lets nobody new in, but whoever joined stays
	require.NoError(t, gate.SetSecret(access.NewSecret()))
	fa, fb = pipe()
	ra = run(NewNegotiator(a, cfg(), fa), binding(2), true)
	rb = run(NewNegotiator(b, hidden(gate), fb), binding(2), false)
	require.NoError(t, (<-ra).err)
	require.NoError(t, (<-rb).err)

	c := newSigner(t)
	fa, fb = pipe()
	ra = run(NewNegotiator(c, withProof(secret), fa), binding(3), true)
	rb = run(NewNegotiator(b, hidden(gate), fb), binding(3), false)
	assert.ErrorIs(t, (<-rb).err, ErrAccessDenied, "the old link must not let a new peer in")
	assert.Error(t, (<-ra).err)

	// removed: needs the current link again
	gate.Remove(pubID(t, a))
	fa, fb = pipe()
	ra = run(NewNegotiator(a, cfg(), fa), binding(4), true)
	rb = run(NewNegotiator(b, hidden(gate), fb), binding(4), false)
	assert.ErrorIs(t, (<-rb).err, ErrAccessDenied)
	assert.Error(t, (<-ra).err)
}

// The owner's contacts call and write without a link.
func TestHiddenNodeAdmitsContacts(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	gate, _ := newHiddenGate(t)
	friend := pubID(t, a)
	gate.SetAllow(func(id types.PeerID) bool { return id == friend })
	fa, fb := pipe()
	ra := run(NewNegotiator(a, cfg(), fa), binding(1), true)
	rb := run(NewNegotiator(b, hidden(gate), fb), binding(1), false)
	require.NoError(t, (<-ra).err)
	require.NoError(t, (<-rb).err)
	assert.Empty(t, gate.Joined(), "a contact is let in, it does not join")
}

// A public node ignores the proof an invited peer sends to everybody it knows a secret for.
func TestPublicNodeIgnoresProof(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	fa, fb := pipe()
	ra := run(NewNegotiator(a, withProof(access.NewSecret()), fa), binding(1), true)
	rb := run(NewNegotiator(b, cfg(), fb), binding(1), false)
	require.NoError(t, (<-ra).err)
	require.NoError(t, (<-rb).err)
}
