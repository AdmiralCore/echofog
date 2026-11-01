package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/stretchr/testify/assert"
)

// How calls go: as set; "auto" is a relay once a proxy is on, which is there to hide the address.
// The NAT services, which show the address, run behind a proxy only for direct calls.
func TestCallRouteBehindProxy(t *testing.T) {
	n := &Node{Cfg: config.DefaultConfig("")}
	assert.Equal(t, config.CallAuto, n.CallRoute())
	assert.True(t, n.natShowsAddr())

	n.Cfg.Network.Proxy = "127.0.0.1:9150"
	assert.Equal(t, config.CallRelay, n.CallRoute(), "auto behind a proxy: a relay")
	assert.False(t, n.natShowsAddr(), "no STUN, port mapping or hole punch behind a proxy")

	n.Cfg.Network.CallRoute = config.CallDirect
	assert.Equal(t, config.CallDirect, n.CallRoute())
	assert.True(t, n.natShowsAddr(), "direct calls need the hole punch")

	n.Cfg.Network.CallRoute = config.CallRelay
	n.Cfg.Network.Proxy = ""
	assert.Equal(t, config.CallRelay, n.CallRoute(), "relay holds without a proxy too")
}
