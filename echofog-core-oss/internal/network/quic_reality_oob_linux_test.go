//go:build linux

package network

import (
	"net"
	"testing"
	"unsafe"

	"github.com/echofog/echofog-core/internal/network/dpi"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type oobRecorder struct {
	dpi.OOBPacketConn
	oob []byte
}

func (r *oobRecorder) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	r.oob = append([]byte(nil), oob...)
	return len(b), len(oob), nil
}

func ect0Cmsg() []byte {
	b := make([]byte, unix.CmsgSpace(1))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = unix.IPPROTO_IP, unix.IP_TOS
	h.SetLen(unix.CmsgLen(1))
	b[unix.CmsgLen(0)] = 2 // ECT(0)
	return b
}

// REALITY packets pass for Chrome's, which carry no ECN marks: a REALITY dial's socket and a
// camouflage node's answers to raw (REALITY) clients leave Not-ECT, as before; noise and plain
// peers keep quic-go's marks.
func TestRealityPacketsLeaveWithoutECN(t *testing.T) {
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer u.Close()
	base, _ := dpi.AsOOB(u)
	to := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	pkt := []byte{0x40, 1, 2, 3}

	write := func(c *QuicRealityConn) []byte {
		rec := &oobRecorder{OOBPacketConn: base}
		c.base = rec
		pc := c.PacketConn().(*quicRealityOOB)
		_, _, err := pc.WriteMsgUDP(pkt, ect0Cmsg(), to)
		require.NoError(t, err)
		return rec.oob
	}

	client := NewQuicRealityConn(u, nil)
	client.browserClient = true
	require.False(t, dpi.HasECNMark(write(client)), "a REALITY dial must not mark ECN")

	noiseNode := perfNode(t, true, false)
	require.True(t, dpi.HasECNMark(write(NewQuicRealityConn(u, noiseNode.tr))), "a noise peer keeps ECN")

	server := NewQuicRealityConn(u, noiseNode.tr)
	server.SetCamouflage([]byte("k"), "")
	require.True(t, dpi.HasECNMark(write(server)), "a camouflage node keeps ECN to noise peers")
	noiseNode.tr.obfs.SetRaw(to.AddrPort(), true)
	require.False(t, dpi.HasECNMark(write(server)), "a camouflage node answers REALITY clients without ECN")
}
