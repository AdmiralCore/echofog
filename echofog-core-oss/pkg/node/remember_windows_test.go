//go:build windows

package node_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/require"
)

// The real thing on Windows: the unlock is sealed with DPAPI for this Windows user, and the next
// start opens the profile with it.
func TestRememberedUnlockWithDPAPI(t *testing.T) {
	require.True(t, node.RememberSupported())
	cfg := rememberProfile(t)
	want := registered(t, cfg, true)

	sealed, err := os.ReadFile(node.RememberFile(cfg))
	require.NoError(t, err)
	require.False(t, bytes.Contains(sealed, []byte("EFU1")), "the record must be sealed, not stored as it is")

	n := node.NewNode(context.Background(), cfg, "auto")
	ask, err := n.LoginRemembered()
	require.NoError(t, err)
	require.True(t, ask)
	got, err := n.IdentityPeerID()
	require.NoError(t, err)
	require.Equal(t, want, got)
	n.Stop()

	// moved into another profile's folder it does not open: the entropy names the profile
	other := rememberProfile(t)
	registered(t, other, false)
	require.NoError(t, os.WriteFile(node.RememberFile(other), sealed, 0o600))
	_, err = node.NewNode(context.Background(), other, "moved").LoginRemembered()
	require.ErrorIs(t, err, node.ErrRememberedForeign)
	require.False(t, node.IsRemembered(other))
}
