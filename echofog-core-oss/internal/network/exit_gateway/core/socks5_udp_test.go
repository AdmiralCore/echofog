package core

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// socksAssociate performs a UDP ASSOCIATE and returns the control connection and relay address.
func socksAssociate(t *testing.T, proxy string) (net.Conn, *net.UDPAddr) {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(6 * time.Second))
	_, err = c.Write([]byte{5, 1, 0})
	require.NoError(t, err)
	_, err = io.ReadFull(c, make([]byte, 2))
	require.NoError(t, err)
	_, err = c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	require.NoError(t, err)
	resp := make([]byte, 10)
	_, err = io.ReadFull(c, resp)
	require.NoError(t, err)
	require.Equal(t, byte(0), resp[1], "socks reply code")
	c.SetDeadline(time.Time{})
	return c, &net.UDPAddr{IP: net.IP(resp[4:8]), Port: int(binary.BigEndian.Uint16(resp[8:]))}
}

func socksUDPPacket(ip net.IP, port int, data []byte) []byte {
	out := []byte{0, 0, 0, 1}
	out = append(out, ip.To4()...)
	out = append(out, byte(port>>8), byte(port))
	return append(out, data...)
}

func readSocksUDP(t *testing.T, pc *net.UDPConn) (net.IP, int, []byte) {
	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := pc.ReadFromUDP(buf)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 10)
	require.Equal(t, byte(1), buf[3])
	return net.IP(append([]byte(nil), buf[4:8]...)), int(binary.BigEndian.Uint16(buf[8:10])), buf[10:n]
}

func dnsQuery(t *testing.T, name string) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}))
	q, err := b.Finish()
	require.NoError(t, err)
	return q
}

func TestParseUDPRequestRejectsFragments(t *testing.T) {
	pkt := socksUDPPacket(net.IPv4(1, 2, 3, 4), 53, []byte("x"))
	addr, raw, data, ok := parseUDPRequest(pkt)
	require.True(t, ok)
	assert.Equal(t, "1.2.3.4:53", addr)
	assert.Equal(t, pkt[3:10], raw)
	assert.Equal(t, []byte("x"), data)

	pkt[2] = 1
	_, _, _, ok = parseUDPRequest(pkt)
	assert.False(t, ok)
	_, _, _, ok = parseUDPRequest([]byte{0, 0, 0, 9})
	assert.False(t, ok)
}
