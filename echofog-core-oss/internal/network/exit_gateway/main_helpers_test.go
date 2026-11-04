package exit_gateway

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const price49 = 49 // micro-USDT per MB, about 5 cents a GB

// echoServer echoes every TCP connection on a loopback listener.
func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

// readWithin waits for one datagram or fails the test.
func readWithin(t *testing.T, u *UDPSession, d time.Duration) ([]byte, string) {
	type res struct {
		b    []byte
		addr string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		b, a, err := u.ReadFrom()
		ch <- res{b, a, err}
	}()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.b, r.addr
	case <-time.After(d):
		t.Fatal("no datagram")
		return nil, ""
	}
}

// udpEchoServer echoes datagrams on a loopback socket.
func udpEchoServer(t *testing.T) *net.UDPConn {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDP(buf[:n], from)
		}
	}()
	return c
}

// bigUDPEcho echoes datagrams of any size.
func bigUDPEcho(t *testing.T) *net.UDPConn {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = c.WriteToUDP(buf[:n], from)
		}
	}()
	return c
}

// openExit is a serving exit that may reach loopback targets (tests).
func openExit() *ExitService {
	svc := NewExitService()
	svc.AllowPrivateTargets(true)
	svc.SetEnabled(true)
	return svc
}
