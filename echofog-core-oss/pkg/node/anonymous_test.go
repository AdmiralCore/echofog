package node

import (
	"context"
	"log/slog"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// Anonymous mode: calls go through relays and the NAT services are off whatever is set; a mode
// that cannot hold (no proxy, no relay pinned) lets the node connect nowhere; the relay mode lets
// it connect to its relay alone; the wallet stays offline unless allowed.
func TestAnonymousMode(t *testing.T) {
	t.Cleanup(func() { network.SetDialOnly(nil); tonwallet.SetNetworkGate(nil) })
	n := &Node{Cfg: config.DefaultConfig(""), Logger: slog.Default()}
	n.Cfg.Network.CallRoute = config.CallDirect
	assert.Equal(t, "", n.AnonMode())
	assert.True(t, n.natShowsAddr())

	n.Cfg.Network.Anonymous = config.AnonTor
	assert.Equal(t, config.CallRelay, n.CallRoute(), "a direct call would show the address")
	assert.False(t, n.natShowsAddr())
	assert.NotEmpty(t, n.AnonBroken(), "Tor with no proxy set")
	n.applyAnonymity()
	assert.True(t, network.DialRefused(context.Background(), "198.51.100.7:443"), "it fails closed")

	n.Cfg.Network.Anonymous = config.AnonRelay
	n.Cfg.AntiDPI.PinnedGuards = []string{"198.51.100.7:443/12D3KooWExample?sni=cover.example&srv=1"}
	assert.Empty(t, n.AnonBroken())
	n.applyAnonymity()
	assert.False(t, network.DialRefused(context.Background(), "198.51.100.7:443"), "its own relay")
	assert.True(t, network.DialRefused(context.Background(), "203.0.113.5:443"), "nothing else")
	assert.False(t, n.admitPunchBehindProxy(types.PeerID{}), "no hole punch")

	n.Cfg.Network.AnonWalletDirect = true
	n.applyAnonymity()
	n.Cfg.Network.Anonymous = ""
	n.applyAnonymity()
	assert.False(t, network.DialRefused(context.Background(), "203.0.113.5:443"), "off: anywhere again")
}
