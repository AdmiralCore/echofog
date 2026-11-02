package natcheck

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func obs(pairs ...string) []Observation {
	var out []Observation
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Observation{From: pairs[i], Addr: ap(pairs[i+1])})
	}
	return out
}

func TestEveryObserverSeesTheSameAddressMeansHolePunchable(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:50000", "b", "8.8.8.7:50000", "c", "8.8.8.7:50000"), nil)
	assert.Equal(t, MappingStable, r.Mapping)
	assert.Equal(t, ap("8.8.8.7:50000"), r.Public)
	assert.False(t, r.NoNAT)
}

func TestDifferentPortsPerDestinationIsSymmetric(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:50000", "b", "8.8.8.7:50123", "c", "8.8.8.7:50321"), nil)
	assert.Equal(t, MappingVaries, r.Mapping)
	assert.Equal(t, "8.8.8.7", r.Public.Addr().String(), "the IP is agreed on even when the ports are not")
}

func TestDifferentIPsPerDestinationIsAlsoHard(t *testing.T) {
	// a carrier-grade NAT with an address pool maps one flow to different public IPs
	r := Classify(obs("a", "8.8.8.7:50000", "b", "8.8.8.9:50000", "c", "8.8.8.11:50000"), nil)
	assert.Equal(t, MappingVaries, r.Mapping)
}

// Two observers that disagree cannot tell a symmetric NAT from one of them lying: the mapping stays
// unknown (the NAT manager then asks more observers), and a tie between two IPs decides nothing. It
// used to go to the lower address as a string: one lying peer out of two chose our address.
func TestOneAgainstOneDecidesNothing(t *testing.T) {
	r := Classify(obs("honest", "9.9.9.9:41000", "liar", "2.16.0.1:1"), nil)
	assert.Equal(t, MappingUnknown, r.Mapping)
	assert.False(t, r.Public.IsValid(), "a tie must not pick an address")
	assert.False(t, r.Agreed(1))

	ports := Classify(obs("honest", "9.9.9.9:41000", "liar", "9.9.9.9:1"), nil)
	assert.Equal(t, MappingUnknown, ports.Mapping, "one disagreeing observer does not make a symmetric NAT")
	assert.Equal(t, "9.9.9.9", ports.Public.Addr().String())
	assert.True(t, ports.Agreed(2), "both saw the IP")
}

// A single observer that disagrees with the others is outvoted: the NAT is a cone.
func TestOneDissenterIsOutvoted(t *testing.T) {
	r := Classify(obs("a", "9.9.9.9:41000", "b", "9.9.9.9:41000", "c", "9.9.9.9:41000", "liar", "2.16.0.1:1"), nil)
	assert.Equal(t, MappingStable, r.Mapping)
	assert.Equal(t, ap("9.9.9.9:41000"), r.Public)
	assert.Equal(t, 3, r.Agree)
	assert.Equal(t, 4, r.Observers)
	assert.True(t, r.Agreed(3))

	// two against two is a draw, whoever they are
	draw := Classify(obs("a", "9.9.9.9:41000", "b", "9.9.9.9:41000", "x", "2.16.0.1:1", "y", "2.16.0.1:1"), nil)
	assert.False(t, draw.Public.IsValid())
	assert.Equal(t, MappingUnknown, draw.Mapping)
}

// An observer has one vote, whatever it answers.
func TestOneVotePerObserver(t *testing.T) {
	r := Classify(obs("liar", "2.16.0.1:1", "liar", "2.16.0.1:1", "liar", "2.16.0.1:2", "honest", "9.9.9.9:41000"), nil)
	assert.False(t, r.Public.IsValid(), "one liar repeating itself is still one against one")
	assert.Equal(t, 2, r.Observers)
}

func TestOneObservationIsNotEnoughToJudge(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:50000"), nil)
	assert.Equal(t, MappingUnknown, r.Mapping)
	assert.Equal(t, ap("8.8.8.7:50000"), r.Public, "still a usable address guess")
}

func TestNothingObserved(t *testing.T) {
	r := Classify(nil, nil)
	assert.Equal(t, MappingUnknown, r.Mapping)
	assert.False(t, r.Public.IsValid())
}

func TestObservedAddressOfOurOwnInterfaceMeansNoNAT(t *testing.T) {
	local := []netip.Addr{netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("8.8.8.7")}
	r := Classify(obs("a", "8.8.8.7:4242", "b", "8.8.8.7:4242"), local)
	assert.True(t, r.NoNAT)

	behind := Classify(obs("a", "8.8.8.7:4242", "b", "8.8.8.7:4242"), []netip.Addr{netip.MustParseAddr("192.168.1.5")})
	assert.False(t, behind.NoNAT)
}

func TestIPv6AnswersDoNotCountAsDisagreement(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:50000", "b", "8.8.8.7:50000", "c", "[2001:4860:4860::8888]:50000"), nil)
	assert.Equal(t, MappingStable, r.Mapping, "a v6 answer is another network, not a different mapping")
	assert.Equal(t, ap("8.8.8.7:50000"), r.Public)

	only6 := Classify(obs("a", "[2001:4860:4860::8888]:50000", "b", "[2001:4860:4860::8888]:50000"), nil)
	assert.Equal(t, MappingStable, only6.Mapping)
}

func TestMajorityDecidesThePublicAddress(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:1111", "b", "8.8.8.7:2222", "c", "8.8.8.7:2222"), nil)
	assert.Equal(t, ap("8.8.8.7:2222"), r.Public)
	assert.Equal(t, MappingStable, r.Mapping, "the one that saw another port is outvoted")
}

func TestIgnoresGarbageObservations(t *testing.T) {
	r := Classify([]Observation{{From: "a"}, {From: "b", Addr: ap("8.8.8.7:0")}}, nil)
	assert.False(t, r.Public.IsValid())
}

// A peer on our own LAN (or inside the same carrier NAT) sees our private address. Next to the
// public observations it must not count as a second mapping: the NAT is a cone, not symmetric.
func TestPrivateObservationsDoNotMakeAConeLookSymmetric(t *testing.T) {
	r := Classify(obs("server1", "8.8.8.7:50000", "server2", "8.8.8.7:50000",
		"lan-peer", "192.168.1.5:4242", "same-carrier", "100.72.1.9:61000"), nil)
	assert.Equal(t, MappingStable, r.Mapping)
	assert.Equal(t, ap("8.8.8.7:50000"), r.Public)

	// a closed network (only private addresses) still gets classified
	closed := Classify(obs("a", "172.18.0.4:4242", "b", "172.18.0.4:4242"), nil)
	assert.Equal(t, MappingStable, closed.Mapping)
	assert.Equal(t, ap("172.18.0.4:4242"), closed.Public)
}

// Behind a symmetric NAT the observed ports show where the NAT allocates from.
func TestSymmetricReportCarriesThePortRange(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:40100", "b", "8.8.8.7:40017", "c", "8.8.8.7:40230",
		"d", "8.8.8.7:40100", "e", "9.9.9.9:9999"), nil)
	assert.Equal(t, MappingVaries, r.Mapping)
	assert.Equal(t, uint16(40017), r.PortLo)
	assert.Equal(t, uint16(40230), r.PortHi)
	assert.Equal(t, 3, r.Ports, "only the ports on the public IP count")

	cone := Classify(obs("a", "8.8.8.7:50000", "b", "8.8.8.7:50000"), nil)
	assert.Equal(t, 1, cone.Ports)
	assert.Equal(t, cone.PortLo, cone.PortHi)
}

// An observer that names our IP with a far-off port must not stretch the range over every port:
// a birthday punch aimed there would be wasted.
func TestALiarCannotStretchThePortRange(t *testing.T) {
	r := Classify(obs("a", "8.8.8.7:40100", "b", "8.8.8.7:40017", "c", "8.8.8.7:40230",
		"liar1", "8.8.8.7:1", "liar2", "8.8.8.7:65535"), nil)
	assert.Equal(t, uint16(40017), r.PortLo)
	assert.Equal(t, uint16(40230), r.PortHi)
	assert.Equal(t, 3, r.Ports)

	// with two answers there is no middle: the range keeps to the agreed port
	two := Classify(obs("a", "8.8.8.7:40100", "liar", "8.8.8.7:1"), nil)
	assert.Equal(t, 1, two.Ports)
	assert.Equal(t, two.PortLo, two.PortHi)

	// a NAT that takes its ports from anywhere keeps its wide range
	wide := Classify(obs("a", "8.8.8.7:2000", "b", "8.8.8.7:30000", "c", "8.8.8.7:61000"), nil)
	assert.Equal(t, uint16(2000), wide.PortLo)
	assert.Equal(t, uint16(61000), wide.PortHi)
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.7": true, "2001:4860:4860::8888": true, "::ffff:8.8.8.8": true, "1.1.1.1": true,
		"192.168.1.1": false, "10.0.0.1": false, "100.64.1.1": false, "100.127.255.1": false,
		"169.254.1.1": false, "127.0.0.1": false, "0.0.0.0": false, "fd00::1": false, "fe80::1": false,
		// reserved ranges an honest observer inside some network may report
		"0.1.2.3": false, "192.0.0.2": false, "192.0.2.1": false, "198.51.100.1": false,
		"203.0.113.7": false, "198.18.0.1": false, "198.19.255.1": false, "240.0.0.1": false,
		"255.255.255.255": false, "224.0.0.1": false, "2001:db8::1": false, "100::1": false,
		"3fff::1": false, "ff02::1": false,
	} {
		assert.Equal(t, want, PublicAddr(netip.MustParseAddr(addr)), addr)
	}
}

// --- STUN over a real socket against fake servers that act like NATs ---

// fakeSTUN answers every binding request with mapped (or with the sender's own address if mapped
// is invalid). It stands in for a STUN server seen through a NAT.
func fakeSTUN(t *testing.T, mapped netip.AddrPort) string {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var req stun.Message
			req.Raw = append([]byte(nil), buf[:n]...)
			if req.Decode() != nil || req.Type != stun.BindingRequest {
				continue
			}
			ip, port := net.IP(mapped.Addr().AsSlice()), int(mapped.Port())
			if !mapped.IsValid() {
				ua := from.(*net.UDPAddr)
				ip, port = ua.IP, ua.Port
			}
			resp, err := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
				&stun.XORMappedAddress{IP: ip, Port: port}, stun.Fingerprint)
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(resp.Raw, from)
		}
	}()
	return pc.LocalAddr().String()
}

func clientSocket(t *testing.T) net.PacketConn {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	return pc
}

func TestQuerySTUNCollectsEveryAnswer(t *testing.T) {
	s1 := fakeSTUN(t, ap("9.9.9.1:40000"))
	s2 := fakeSTUN(t, ap("9.9.9.1:40000"))
	got, err := QuerySTUN(context.Background(), clientSocket(t), []string{s1, s2}, 2*time.Second)
	require.NoError(t, err)
	require.Len(t, got, 2)
	r := Classify(got, nil)
	assert.Equal(t, MappingStable, r.Mapping)
	assert.Equal(t, ap("9.9.9.1:40000"), r.Public)
}

func TestQuerySTUNDetectsASymmetricNAT(t *testing.T) {
	s1 := fakeSTUN(t, ap("9.9.9.1:40000"))
	s2 := fakeSTUN(t, ap("9.9.9.1:40001")) // another port for every destination
	s3 := fakeSTUN(t, ap("9.9.9.1:40002"))
	got, err := QuerySTUN(context.Background(), clientSocket(t), []string{s1, s2, s3}, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, MappingVaries, Classify(got, nil).Mapping)
}

func TestQuerySTUNSurvivesADeadServer(t *testing.T) {
	alive := fakeSTUN(t, ap("9.9.9.1:40000"))
	dead := "127.0.0.1:1" // nothing listens: UDP just vanishes
	start := time.Now()
	got, err := QuerySTUN(context.Background(), clientSocket(t), []string{dead, alive}, 500*time.Millisecond)
	require.NoError(t, err, "one live server is enough")
	assert.Len(t, got, 1)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestQuerySTUNFailsWhenNothingAnswers(t *testing.T) {
	_, err := QuerySTUN(context.Background(), clientSocket(t), []string{"127.0.0.1:1", "127.0.0.1:2"}, 300*time.Millisecond)
	assert.ErrorIs(t, err, ErrNoSTUNAnswer)
}

func TestQuerySTUNIgnoresAnswersToRequestsItNeverSent(t *testing.T) {
	// an attacker on the path spraying forged binding responses must not be believed
	c := clientSocket(t)
	forged, err := stun.Build(stun.TransactionID, stun.BindingSuccess, &stun.XORMappedAddress{IP: net.ParseIP("6.6.6.6"), Port: 666})
	require.NoError(t, err)
	atk, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer atk.Close()
	go func() {
		for i := 0; i < 20; i++ {
			_, _ = atk.WriteTo(forged.Raw, c.LocalAddr())
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, err = QuerySTUN(context.Background(), c, []string{"127.0.0.1:1"}, 400*time.Millisecond)
	assert.ErrorIs(t, err, ErrNoSTUNAnswer)
}

func TestQuerySTUNRespectsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := QuerySTUN(ctx, clientSocket(t), []string{"127.0.0.1:1"}, 10*time.Second)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// Run with NATCHECK_LIVE=1 to check the default public servers still answer from this network.
func TestLiveDefaultServers(t *testing.T) {
	if os.Getenv("NATCHECK_LIVE") == "" {
		t.Skip("set NATCHECK_LIVE=1 to query the real STUN servers")
	}
	c, err := net.ListenPacket("udp4", ":0")
	require.NoError(t, err)
	defer c.Close()
	got, err := QuerySTUN(context.Background(), c, DefaultSTUNServers, 4*time.Second)
	require.NoError(t, err)
	for _, o := range got {
		t.Logf("%-32s sees us as %s", o.From, o.Addr)
	}
	r := Classify(got, LocalAddrs())
	t.Logf("mapping: %s, public: %s, no NAT: %v", r.Mapping, r.Public, r.NoNAT)
	answered := map[string]bool{}
	for _, o := range got {
		answered[o.From] = true
	}
	for _, s := range DefaultSTUNServers {
		if !answered[s] {
			t.Logf("NO ANSWER from %s", s)
		}
	}
}

// An observer on another socket of ours tells the IP, not the port: its connection has a NAT
// mapping of its own.
func TestIPOnlyObservationsCountForTheIPAlone(t *testing.T) {
	r := Classify([]Observation{
		{From: "guard1", Addr: ap("8.8.8.7:61001"), IPOnly: true},
		{From: "guard2", Addr: ap("8.8.8.7:61877"), IPOnly: true},
	}, nil)
	assert.Equal(t, "8.8.8.7", r.Public.Addr().String())
	assert.Zero(t, r.Public.Port())
	assert.Equal(t, MappingUnknown, r.Mapping)
	assert.Zero(t, r.Ports)
	assert.True(t, r.Agreed(2))

	// next to a peer on the main socket they back its IP; the port and mapping are the main one's
	mixed := Classify([]Observation{
		{From: "peer", Addr: ap("8.8.8.7:4242")},
		{From: "guard1", Addr: ap("8.8.8.7:61001"), IPOnly: true},
		{From: "guard2", Addr: ap("8.8.8.7:61877"), IPOnly: true},
	}, nil)
	assert.Equal(t, ap("8.8.8.7:4242"), mixed.Public)
	assert.Equal(t, 3, mixed.Agree)
	assert.Equal(t, MappingUnknown, mixed.Mapping, "one port-bearing observer cannot judge the mapping")
	assert.Equal(t, 1, mixed.Ports)
}
