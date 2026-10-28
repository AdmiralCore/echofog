package access

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// device is a peer that proves secrets in the handshake like a real app would.
type device struct {
	pub []byte
	id  types.PeerID
}

func newDevice(t *testing.T) device {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	id, ok := peerIDOf(pub)
	require.True(t, ok)
	return device{pub: pub, id: id}
}

// joins runs one inbound handshake of d proving secret.
func (d device) joins(g *Gate, secret []byte) bool {
	binding, nonce := []byte("connection"), []byte("nonce")
	return g.AdmitHandshake(d.pub, binding, nonce, HandshakeProof(secret, binding, nonce, d.pub))
}

func gatedWithPasses(t *testing.T) (*Gate, *Passbook, *time.Time) {
	t.Helper()
	g := NewGate()
	require.NoError(t, g.SetSecret(NewSecret()))
	g.Enable(true)
	b := NewPassbook()
	now := time.Unix(1_800_000_000, 0)
	b.now = func() time.Time { return now }
	g.SetPasses(b)
	return g, b, &now
}

// A pass lets in the devices that prove its own secret, up to its number of devices; another pass's
// secret or a made-up one does not.
func TestPassLetsItsDevicesIn(t *testing.T) {
	g, b, _ := gatedWithPasses(t)
	p, err := b.Create(PassSpec{Label: "order 17", Days: 30, MaxDevices: 2})
	require.NoError(t, err)
	other, err := b.Create(PassSpec{})
	require.NoError(t, err)

	a, c, d := newDevice(t), newDevice(t), newDevice(t)
	assert.False(t, a.joins(g, NewSecret()), "a made-up secret")
	assert.True(t, a.joins(g, p.Secret))
	assert.True(t, c.joins(g, p.Secret))
	assert.False(t, d.joins(g, p.Secret), "the third device of a two-device pass")
	assert.True(t, g.Allowed(a.id))

	got, _ := b.Get(p.ID)
	assert.Len(t, got.Devices, 2)
	assert.Equal(t, "order 17", got.Label)
	_ = a.joins(g, other.Secret)
	still, _ := b.PassOf(a.id)
	assert.Equal(t, p.ID, still.ID, "a device stays with its pass")
	got, _ = b.Get(other.ID)
	assert.Empty(t, got.Devices)
}

// A pass that ended, ran out of traffic or was revoked shuts its devices out; extending it lets them
// back in, the term counting on from where it ended.
func TestPassEndsAndIsExtended(t *testing.T) {
	g, b, now := gatedWithPasses(t)
	p, _ := b.Create(PassSpec{Days: 1, QuotaGB: 1})
	a := newDevice(t)
	require.True(t, a.joins(g, p.Secret))

	*now = now.Add(25 * time.Hour)
	assert.False(t, g.Allowed(a.id), "expired")
	assert.Equal(t, []types.PeerID{a.id}, b.Shut())
	assert.False(t, a.joins(g, p.Secret), "an expired pass's secret lets nobody in")

	p, _ = b.Extend(p.ID, PassSpec{Days: 30}, false)
	assert.True(t, g.Allowed(a.id))
	assert.Empty(t, b.Shut())
	assert.Equal(t, now.Add(30*24*time.Hour).Unix(), p.Expires, "an ended pass counts from now")

	assert.True(t, b.Charge(a.id, gb-1))
	assert.True(t, b.Charge(a.id, 10))
	assert.False(t, b.Charge(a.id, 1), "the allowance is spent")
	assert.False(t, g.Allowed(a.id))
	p, _ = b.Extend(p.ID, PassSpec{QuotaGB: 1}, false)
	assert.Equal(t, 2*gb, p.QuotaBytes)
	assert.Equal(t, gb, p.UsedBytes, "the overrun past the old allowance is not owed")
	assert.True(t, g.Allowed(a.id))

	_, _ = b.Revoke(p.ID)
	assert.False(t, g.Allowed(a.id), "revoked")
	_, _ = b.Extend(p.ID, PassSpec{}, true)
	assert.True(t, g.Allowed(a.id), "extending lifts a revocation")
	p, _ = b.Get(p.ID)
	assert.Zero(t, p.UsedBytes, "the count starts again")

	require.NoError(t, b.Delete(p.ID))
	assert.False(t, g.Allowed(a.id), "a deleted pass's device is a stranger again")
	assert.True(t, b.Charge(a.id, 1), "a peer of no pass is not counted")
}

// The shared link keeps working next to passes, and passes survive a reload.
func TestPassesBesideTheSharedSecretAndReloaded(t *testing.T) {
	g, b, _ := gatedWithPasses(t)
	shared := newDevice(t)
	assert.True(t, shared.joins(g, g.Secret()))
	assert.Len(t, g.Joined(), 1)

	p, _ := b.Create(PassSpec{Days: 7})
	a := newDevice(t)
	require.True(t, a.joins(g, p.Secret))

	again := NewPassbook()
	again.Load(b.List())
	g.SetPasses(again)
	assert.True(t, g.Allowed(a.id))
	got, err := again.Get(p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.Secret, got.Secret)

	_, err = b.Create(PassSpec{MaxDevices: 101})
	assert.ErrorIs(t, err, ErrBadPass)
	_, err = b.Create(PassSpec{Days: -1})
	assert.ErrorIs(t, err, ErrBadPass)
}
