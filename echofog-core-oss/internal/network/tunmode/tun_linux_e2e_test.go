//go:build linux && tune2e

// Real TUN end to end; needs root and /dev/net/tun (see `make test-tun`, which runs it in Docker
// with --cap-add NET_ADMIN --device /dev/net/tun --add-host echo.test:127.0.0.1).
package tunmode

import (
	"context"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/network/fakedns"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

func newSwarm(t *testing.T) (*swarm.Swarm, *network.QuicTransport) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	im, store := testutil.SetupTestIdentity(t)
	t.Cleanup(store.CloseAndClear)
	account, err := im.GetActiveAccount()
	require.NoError(t, err)
	_, pub, err := account.GetIdentityKey()
	require.NoError(t, err)
	var pubKey types.PeerPublicKey
	copy(pubKey[:], pub)
	_, tls := testutil.GetRandomKeyAndTls(t)
	tr, err := network.NewQUICTransport("127.0.0.1:0", tls, network.GetQuicConfig(), im, config.CurrentProtocolVersion)
	require.NoError(t, err)
	require.NoError(t, tr.ListenEarly(ctx))
	t.Cleanup(func() { tr.Close() })
	d := dispatcher.NewDispatcher(ctx)
	go d.Start()
	sw := swarm.NewSwarm(ctx, tr, types.PeerPubKeyToID(pubKey), d, im, tr.ConnChan(), store.St, config.DefaultConfig(""))
	t.Cleanup(sw.Close)
	return sw, tr
}

func sh(t *testing.T, name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
}

func TestLinuxTunEndToEnd(t *testing.T) {
	// exit and client in one process; every target lives on loopback so the exit's own
	// connections never enter the TUN (on a real exit they are on another machine)
	exitSwarm, exitTr := newSwarm(t)
	clientSwarm, clientTr := newSwarm(t)
	svc := exit_gateway.NewExitService()
	svc.AllowPrivateTargets(true)
	svc.SetEnabled(true)
	exit_gateway.AttachToSwarm(exitSwarm, svc)
	require.NoError(t, clientTr.DialEarly(context.Background(), exitTr.Addr().String()))
	require.Eventually(t, func() bool { return len(clientSwarm.GetAllPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	socks := exit_gateway.NewSOCKS5Server("127.0.0.1:0", exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(clientSwarm, clientSwarm.GetAllPeers()[0].ID())))
	socks.SetFakeDNS(fakedns.New())
	require.NoError(t, socks.Start())
	t.Cleanup(func() { socks.Stop() })

	ctl := &Controller{launch: func(_ string, args []string) error {
		go HelperMain(args[1:])
		return nil
	}}
	require.NoError(t, ctl.Start(context.Background(), socks.Addr().String(), clientTr.UDPPort()))
	rules := sh(t, "ip", "rule")
	t.Log(rules)
	assert.Contains(t, rules, "9001:")
	assert.Contains(t, rules, "sport "+strconv.Itoa(clientTr.UDPPort()))
	assert.Contains(t, sh(t, "ip", "addr", "show", DeviceName), "198.18.0.1/15")

	// 1. DNS through the TUN to the fake resolver address
	dns, err := net.Dial("udp", "198.18.0.2:53")
	require.NoError(t, err)
	defer dns.Close()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 1, RecursionDesired: true})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("echo.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}))
	q, _ := b.Finish()
	_, err = dns.Write(q)
	require.NoError(t, err)
	dns.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, err := dns.Read(buf)
	require.NoError(t, err, "no DNS answer through the TUN")
	var msg dnsmessage.Message
	require.NoError(t, msg.Unpack(buf[:n]))
	require.Len(t, msg.Answers, 1)
	a := msg.Answers[0].Body.(*dnsmessage.AResource)
	fakeIP := net.IP(a.A[:])
	require.True(t, fakedns.IsFake(fakeIP))
	t.Log("echo.test ->", fakeIP)

	// 2. TCP to the fake IP: TUN -> tun2socks -> SOCKS -> exit -> echo.test (127.0.0.1)
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer tl.Close()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	tport := tl.Addr().(*net.TCPAddr).Port
	c, err := net.DialTimeout("tcp", net.JoinHostPort(fakeIP.String(), strconv.Itoa(tport)), 5*time.Second)
	require.NoError(t, err)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write([]byte("tcp through tun"))
	require.NoError(t, err)
	got := make([]byte, 15)
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, "tcp through tun", string(got))
	c.Close()

	// 3. UDP to the fake IP comes back from the fake IP
	ul, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer ul.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, from, err := ul.ReadFromUDP(b)
			if err != nil {
				return
			}
			ul.WriteToUDP(b[:n], from)
		}
	}()
	u, err := net.Dial("udp", net.JoinHostPort(fakeIP.String(), strconv.Itoa(ul.LocalAddr().(*net.UDPAddr).Port)))
	require.NoError(t, err)
	defer u.Close()
	var ok bool
	for i := 0; i < 5 && !ok; i++ { // the first datagram also sets the association up
		u.Write([]byte("udp through tun"))
		u.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := u.Read(buf); err == nil {
			assert.Equal(t, "udp through tun", string(buf[:n]))
			ok = true
		}
	}
	assert.True(t, ok, "no UDP echo through the TUN")

	// 4. off: the device and the rules are gone
	ctl.Stop()
	require.Eventually(t, func() bool {
		return !strings.Contains(sh(t, "ip", "rule"), "9001:")
	}, 5*time.Second, 100*time.Millisecond, "policy rules must be removed")
	require.Eventually(t, func() bool {
		_, err := net.InterfaceByName(DeviceName)
		return err != nil
	}, 5*time.Second, 100*time.Millisecond, "TUN device must be removed")
}
