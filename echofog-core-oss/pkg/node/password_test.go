package node_test

import (
	"context"
	"errors"
	"testing"

	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// The new password opens the same identity and the old one no longer does; the unlock remembered
// on this computer follows, with its payments setting.
func TestChangePassword(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	want := registered(t, cfg, true)

	n := node.NewNode(context.Background(), cfg, "change")
	require.NoError(t, n.Login([]byte("right-pass")))
	require.ErrorIs(t, n.ChangePassword([]byte("typo"), []byte("new-pass-1")), node.ErrWrongPassword)
	require.Error(t, n.ChangePassword([]byte("right-pass"), nil))
	require.NoError(t, n.ChangePassword([]byte("right-pass"), []byte("new-pass-1")))
	require.NoError(t, n.VerifyPassword([]byte("new-pass-1")), "the running node takes the new password at once")
	require.ErrorIs(t, n.VerifyPassword([]byte("right-pass")), node.ErrWrongPassword)
	n.Stop()

	require.Error(t, node.NewNode(context.Background(), cfg, "old").Login([]byte("right-pass")))
	n = node.NewNode(context.Background(), cfg, "new")
	require.NoError(t, n.Login([]byte("new-pass-1")))
	got, err := n.IdentityPeerID()
	require.NoError(t, err)
	require.Equal(t, want, got, "the same identity under the new password")
	n.Stop()

	n = node.NewNode(context.Background(), cfg, "auto")
	ask, err := n.LoginRemembered()
	require.NoError(t, err, "the remembered unlock follows the new password")
	require.True(t, ask)
	n.Stop()
}

// A profile that is not remembered stays so.
func TestChangePasswordRemembersNothingNew(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	n := node.NewNode(context.Background(), cfg, "reg")
	_, err := n.Register([]byte("right-pass"), []byte(""))
	require.NoError(t, err)
	require.NoError(t, n.ChangePassword([]byte("right-pass"), []byte("new-pass-1")))
	require.False(t, node.IsRemembered(cfg))
	n.Stop()
}

// sealFails opens what fakeProtector sealed but cannot seal anything new.
type sealFails struct{ fakeProtector }

func (sealFails) Protect([]byte, []byte) ([]byte, error) { return nil, errors.New("no protector") }

// When the remembered unlock cannot be sealed with the new password, it is dropped rather than
// left to fail at the next start; the password change itself stands.
func TestChangePasswordDropsARememberedUnlockItCannotReseal(t *testing.T) {
	defer node.SetRememberProtector(fakeProtector{user: "alice"})()
	cfg := rememberProfile(t)
	registered(t, cfg, false)

	n := node.NewNode(context.Background(), cfg, "change")
	require.NoError(t, n.Login([]byte("right-pass")))
	node.SetRememberProtector(sealFails{fakeProtector{user: "alice"}})
	require.NoError(t, n.ChangePassword([]byte("right-pass"), []byte("new-pass-1")))
	require.False(t, node.IsRemembered(cfg))
	n.Stop()

	n = node.NewNode(context.Background(), cfg, "new")
	require.NoError(t, n.Login([]byte("new-pass-1")))
	n.Stop()
}
