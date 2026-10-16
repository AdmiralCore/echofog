package handshake

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitiatorAnnouncesItsListenPort(t *testing.T) {
	a, b := pipe()
	ic := cfg()
	ic.ListenPort = 443 // Docker publishes 443 to the container's 4242: the port peers must dial
	ini := run(NewNegotiator(newSigner(t), ic, a), binding(7), true)
	resp := run(NewNegotiator(newSigner(t), cfg(), b), binding(7), false)

	r := <-resp
	require.NoError(t, r.err)
	require.Equal(t, uint16(443), r.res.ListenPort, "the responder learns where the initiator accepts connections")
	require.NoError(t, (<-ini).err)
}

func TestNoListenPortAnnouncedIsZero(t *testing.T) {
	a, b := pipe()
	ini := run(NewNegotiator(newSigner(t), cfg(), a), binding(3), true)
	r := <-run(NewNegotiator(newSigner(t), cfg(), b), binding(3), false)
	require.NoError(t, r.err)
	require.Zero(t, r.res.ListenPort)
	require.NoError(t, (<-ini).err)
	require.Zero(t, listenPortOf(70000), "a value that is no port is ignored")
}
