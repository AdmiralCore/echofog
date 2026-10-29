package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A relay link pasted in its https form (from a messenger) is read as the address it carries.
func TestRelayAddrFromWebLink(t *testing.T) {
	const id = "98466572ebb322301eec59937bebc6677023c909471344ff493d036dbb9f8ad0"
	link := MakeRelayLink("34.32.224.253:443/" + id + "?sni=aifraud.ru&srv=1")
	info, err := ParseRelayAddrInfo(access.WebLink(link))
	require.NoError(t, err)
	assert.Equal(t, "34.32.224.253:443", info.Addr)
	assert.Equal(t, id, info.ID.String())
	assert.Equal(t, "aifraud.ru", info.SNI)
	assert.True(t, info.Server)
}
