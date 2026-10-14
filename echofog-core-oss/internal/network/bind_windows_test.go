package network

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBindUDPToInterface(t *testing.T) {
	c, err := net.ListenUDP("udp", nil) // dual-stack, like the node's socket
	require.NoError(t, err)
	defer c.Close()

	ifs, err := net.Interfaces()
	require.NoError(t, err)
	var idx int
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp != 0 {
			idx = ifc.Index
			break
		}
	}
	require.NotZero(t, idx)
	require.NoError(t, bindUDPToInterface(c, idx))
	require.NoError(t, bindUDPToInterface(c, 0))
}
