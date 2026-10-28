package node

import (
	"context"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/stretchr/testify/require"
)

// fakeScan replaces the neighbour scan so tests never touch the network; found switches the result.
func fakeScan(t *testing.T, found *atomic.Bool) {
	t.Helper()
	prev, prevEvery, prevIP, prevWide := scanForNeighbor, mimicryRetryEvery, localIPv4, scanWide
	scanWide = func(context.Context, netip.Addr, mimicry.ServerProfile, *slog.Logger) (mimicry.Neighbor, bool) {
		return mimicry.Neighbor{}, false
	}
	localIPv4 = func() (netip.Addr, bool) { return netip.MustParseAddr("203.0.113.9"), true }
	scanForNeighbor = func(context.Context, netip.Addr, mimicry.ServerProfile, *slog.Logger) (mimicry.Neighbor, bool) {
		if found.Load() {
			return mimicry.Neighbor{ServerName: "neighbour.example.net", Addr: "192.0.2.10:443"}, true
		}
		return mimicry.Neighbor{}, false
	}
	mimicryRetryEvery = 50 * time.Millisecond
	t.Cleanup(func() { scanForNeighbor, mimicryRetryEvery, localIPv4, scanWide = prev, prevEvery, prevIP, prevWide })
}

func publicNode(t *testing.T) *Node {
	t.Helper()
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	// public as far as the anti-DPI rule is concerned; the test never binds it (see below)
	cfg.Network.TCPListenAddr = "0.0.0.0:0"
	cfg.Storage.DatabasePath = t.TempDir() + "/db"
	cfg.Network.BootstrapNodes = nil
	n := NewNode(context.Background(), cfg, "public")
	_, err := n.Register([]byte("pass123"), []byte(""))
	require.NoError(t, err)
	return n
}

// With no camouflage site the node must still run (QUIC needs none), only without the public TCP
// listener — refusing to start the whole node would drop a perfectly good UDP relay.
func TestPublicNodeWithoutNeighbourStartsOnUDPOnly(t *testing.T) {
	var found atomic.Bool
	fakeScan(t, &found)
	n := publicNode(t)
	require.NoError(t, n.Start(), "no neighbour must not stop the node")
	t.Cleanup(n.Stop)
	require.NotNil(t, n.Transport.Addr(), "QUIC is up")
	require.Nil(t, n.Net.TCP.Addr(), "the public TCP listener waits for a camouflage site")
}

// Once a neighbour turns up, the TCP listener comes on without a restart, mimicking it.
func TestRetryTurnsTCPOnWhenANeighbourIsFound(t *testing.T) {
	var found atomic.Bool
	fakeScan(t, &found)
	// The retry Start launches would listen on "0.0.0.0:0", an outside interface (a firewall
	// prompt on Windows): keep it asleep and drive one on loopback instead.
	mimicryRetryEvery = time.Hour
	n := publicNode(t)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	require.Nil(t, n.Net.TCP.Addr())

	go n.retryMimicryTarget("127.0.0.1:0", 20*time.Millisecond)
	found.Store(true)
	require.Eventually(t, func() bool { return n.Net.TCP.Addr() != nil }, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "neighbour.example.net", n.Cfg.AntiDPI.TLSServerName)
	require.Equal(t, "192.0.2.10:443", n.Cfg.AntiDPI.CamouflageAddr)
}

// On a cloud machine or in Docker the interface has a private address; the neighbours are then
// looked for around the public address the operator gave for the invitation link.
func TestNeighbourScanUsesThePublicAddress(t *testing.T) {
	prev := localIPv4
	t.Cleanup(func() { localIPv4 = prev })

	n := &Node{Cfg: config.DefaultConfig("")}
	localIPv4 = func() (netip.Addr, bool) { return netip.MustParseAddr("10.164.0.7"), true }
	_, ok := n.neighbourScanIP()
	require.False(t, ok, "a private interface address and nothing else: no scan")

	n.Cfg.Access.PublicAddrs = []string{"vpn.example.org:443", "34.158.189.116:443"}
	ip, ok := n.neighbourScanIP()
	require.True(t, ok)
	require.Equal(t, "34.158.189.116", ip.String())

	localIPv4 = func() (netip.Addr, bool) { return netip.MustParseAddr("203.0.113.9"), true }
	ip, _ = n.neighbourScanIP()
	require.Equal(t, "203.0.113.9", ip.String(), "a public interface address wins")
}

// When nothing nearby fits, the background retry searches the whole hosting block, but not more
// often than wideScanEvery; a site found that way is adopted like a near one.
func TestRetrySearchesWiderWhenNothingIsNear(t *testing.T) {
	var found atomic.Bool
	fakeScan(t, &found)
	var wide atomic.Int32
	var wideFinds atomic.Bool
	scanWide = func(context.Context, netip.Addr, mimicry.ServerProfile, *slog.Logger) (mimicry.Neighbor, bool) {
		wide.Add(1)
		if wideFinds.Load() {
			return mimicry.Neighbor{ServerName: "far.example.net", Addr: "192.0.2.99:443"}, true
		}
		return mimicry.Neighbor{}, false
	}
	mimicryRetryEvery = time.Hour // the test drives the retries itself
	n := publicNode(t)
	require.NoError(t, n.Start())
	t.Cleanup(n.Stop)
	require.Zero(t, wide.Load(), "start-up never runs the wide search")
	n.ensureMimicryTarget(true)
	n.ensureMimicryTarget(true)
	require.Equal(t, int32(1), wide.Load(), "the wide search runs at most every wideScanEvery")
	require.Empty(t, n.Cfg.AntiDPI.TLSServerName)

	n.wideScanAt = time.Now().Add(-wideScanEvery)
	wideFinds.Store(true)
	n.ensureMimicryTarget(true)
	require.Equal(t, int32(2), wide.Load())
	require.Equal(t, "far.example.net", n.Cfg.AntiDPI.TLSServerName)
	require.Equal(t, "192.0.2.99:443", n.Cfg.AntiDPI.CamouflageAddr)
}
