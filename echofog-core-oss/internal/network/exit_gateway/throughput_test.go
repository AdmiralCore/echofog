package exit_gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// proxyPair is a client and an exit joined over real QUIC on loopback, with the client's SOCKS5
// proxy in front: the whole path an app's connection takes.
func proxyPair(t testing.TB) (proxy string, svc *ExitService) {
	tt, ok := t.(*testing.T)
	if !ok {
		t.Fatal("proxyPair needs *testing.T")
	}
	exitSwarm, exitTr := newSwarm(tt)
	clientSwarm, clientTr := newSwarm(tt)
	svc = NewExitService()
	svc.allowPrivate = true
	svc.SetEnabled(true)
	AttachToSwarm(exitSwarm, svc)

	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool {
		return len(exitSwarm.GetAllPeers()) > 0 && len(clientSwarm.GetAllPeers()) > 0
	}, 5*time.Second, 50*time.Millisecond)

	srv := NewSOCKS5Server("127.0.0.1:0", NewP2PDialer(SwarmOpener(clientSwarm, clientSwarm.GetAllPeers()[0].ID())))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { srv.Stop() })
	return srv.Addr().String(), svc
}

// socksDial opens a CONNECT through the proxy without the test deadline socksConnect sets.
func socksDial(proxy, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		c.Close()
		return nil, err
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		return nil, err
	}
	host, port, _ := net.SplitHostPort(target)
	var p uint16
	fmt.Sscan(port, &p)
	req := append([]byte{5, 1, 0, 1}, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, p)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 10)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks reply %d", resp[1])
	}
	return c, nil
}

// server runs handle for every connection on a loopback listener.
func server(t testing.TB, handle func(net.Conn)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); handle(c) }()
		}
	}()
	return ln.Addr().String()
}

// Every byte arrives intact both ways, and a client that finishes sending (half-close) still gets
// the whole answer: an HTTP/1.0 upload, rsync or a "send then wait for the reply" protocol.
func TestProxyIntegrityAndHalfClose(t *testing.T) {
	proxy, _ := proxyPair(t)
	// reads everything until EOF, then answers with the sha256 of what it got and a tail of data
	target := server(t, func(c net.Conn) {
		h := sha256.New()
		if _, err := io.Copy(h, c); err != nil {
			return
		}
		c.Write(h.Sum(nil))
		c.Write(bytes.Repeat([]byte{7}, 1<<20))
	})

	c, err := socksDial(proxy, target)
	require.NoError(t, err)
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))

	payload := make([]byte, 8<<20)
	_, _ = rand.Read(payload)
	_, err = c.Write(payload)
	require.NoError(t, err)
	require.NoError(t, c.(*net.TCPConn).CloseWrite(), "done sending")

	got, err := io.ReadAll(c)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(got), 32, "no answer after half-close")
	want := sha256.Sum256(payload)
	require.Equal(t, want[:], got[:32], "uploaded data corrupted")
	require.Equal(t, 1<<20, len(got)-32, "download tail cut short")
}

// The exit closing its side reaches the app as EOF, not a hang until some idle timeout.
func TestProxyRemoteCloseReachesClient(t *testing.T) {
	proxy, _ := proxyPair(t)
	target := server(t, func(c net.Conn) { c.Write([]byte("bye")) })
	c, err := socksDial(proxy, target)
	require.NoError(t, err)
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	require.NoError(t, err, "the close must arrive before the deadline")
	require.Equal(t, "bye", string(got))
}

// TestProxyThroughput measures the path on loopback. It runs only with ECHOFOG_BENCH=1, since
// numbers on a shared CI runner say little:
//
//	ECHOFOG_BENCH=1 go test ./internal/network/exit_gateway -run TestProxyThroughput -v
func TestProxyThroughput(t *testing.T) {
	if os.Getenv("ECHOFOG_BENCH") == "" {
		t.Skip("set ECHOFOG_BENCH=1 to measure")
	}
	const size = 256 << 20
	chunk := make([]byte, 256<<10)
	_, _ = rand.Read(chunk)
	sink := server(t, func(c net.Conn) {
		n, _ := io.Copy(io.Discard, c)
		binary.Write(c, binary.BigEndian, n)
	})
	source := server(t, func(c net.Conn) {
		for sent := 0; sent < size; sent += len(chunk) {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	})

	run := func(name string, dial func(string) (net.Conn, error)) {
		// download
		c, err := dial(source)
		require.NoError(t, err)
		start := time.Now()
		n, _ := io.Copy(io.Discard, c)
		down := float64(n) * 8 / 1e6 / time.Since(start).Seconds()
		c.Close()

		// upload
		c, err = dial(sink)
		require.NoError(t, err)
		start = time.Now()
		for sent := 0; sent < size; sent += len(chunk) {
			if _, err := c.Write(chunk); err != nil {
				break
			}
		}
		c.(*net.TCPConn).CloseWrite()
		var got int64
		binary.Read(c, binary.BigEndian, &got)
		up := float64(got) * 8 / 1e6 / time.Since(start).Seconds()
		c.Close()

		// 8 downloads at once
		var wg sync.WaitGroup
		var mu sync.Mutex
		var total int64
		start = time.Now()
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := dial(source)
				if err != nil {
					return
				}
				defer c.Close()
				n, _ := io.Copy(io.Discard, c)
				mu.Lock()
				total += n
				mu.Unlock()
			}()
		}
		wg.Wait()
		par := float64(total) * 8 / 1e6 / time.Since(start).Seconds()

		// connection setup
		echo := echoServer(t)
		var lat []time.Duration
		for i := 0; i < 40; i++ {
			s := time.Now()
			c, err := dial(echo)
			if err != nil {
				continue
			}
			c.Write([]byte{1})
			c.Read(make([]byte, 1))
			lat = append(lat, time.Since(s))
			c.Close()
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p50 := time.Duration(0)
		if len(lat) > 0 {
			p50 = lat[len(lat)/2]
		}
		t.Logf("%-8s down %7.0f Mbit/s   up %7.0f Mbit/s   8x down %7.0f Mbit/s   connect+first byte p50 %v",
			name, down, up, par, p50.Round(10*time.Microsecond))
	}

	run("direct", func(a string) (net.Conn, error) { return net.Dial("tcp", a) })
	proxy, _ := proxyPair(t)
	run("proxied", func(a string) (net.Conn, error) { return socksDial(proxy, a) })
}
