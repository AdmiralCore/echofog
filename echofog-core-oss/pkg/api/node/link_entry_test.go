package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The invitation link of a public server can go wherever an address goes (the relay and bootstrap
// lists): it becomes host:port/<peer id> with the SNI and the server role kept.
func TestALinkBecomesAnAddressEntry(t *testing.T) {
	link := "echofog://join?v=1&net=echofog&p=102&id=5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d" +
		"&sni=d-hp-integrationdb-stag.hp-infra.net&srv=1&a=34.158.189.116%3A443" +
		"&r=34.32.224.253%3A443%2F98466572ebb322301eec59937bebc6677023c909471344ff493d036dbb9f8ad0%3Fsni%3Daifraud.ru%26srv%3D1"
	got, err := linkEntry(link)
	require.NoError(t, err)
	info, err := ParseRelayAddrInfo(got)
	require.NoError(t, err)
	assert.Equal(t, "34.158.189.116:443", info.Addr)
	assert.Equal(t, "5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d", info.ID.String())
	assert.Equal(t, "d-hp-integrationdb-stag.hp-infra.net", info.SNI)
	assert.True(t, info.Server)

	// an address is left as it is
	plain := "1.2.3.4:443/5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d"
	got, err = linkEntry(plain)
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	// a hidden node's link carries its secret: it is joined, never listed
	_, err = linkEntry("echofog://join?v=1&net=echofog&id=5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d&s=AAAAAAAAAAAAAAAAAAAAAA&a=1.2.3.4%3A443")
	assert.Error(t, err)
}
