package natcheck

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/pion/stun"
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
	r := Classify(obs("a", "203.0.113.7:50000", "b", "203.0.113.7:50000", "c", "203.0.113.7:50000"), nil)
	assert.Equal(t, MappingStable, r.Mapping)
	assert.Equal(t, ap("203.0.113.7:50000"), r.Public)
	assert.False(t, r.NoNAT)
}

func TestDifferentPortsPerDestinationIsSymmetric(t *testing.T) {
	r := Classify(obs("a", "203.0.113.7:50000", "b", "203.0.113.7:50123"), nil)
	assert.Equal(t, MappingVaries, r.Mapping)
}

func TestDifferentIPsPerDestinationIsAlsoHard(t *testing.T) {
	// a carrier-grade NAT with an address pool maps one flow to different public IPs
	r := Classify(obs("a", "203.0.113.7:50000", "b", "203.0.113.9:50000"), nil)
	assert.Equal(t, MappingVaries, r.Mapping)
}

func TestOneObservationIsNotEnoughToJudge(t *testing.T) {
	r := Classify(obs("a", "203.0.113.7:50000"), nil)
	assert.Equal(t, MappingUnknown, r.Mapping)
	assert.Equal(t, ap("203.0.113.7:50000"), r.Public, "still a usable address guess")
}

func TestNothingObserved(t *testing.T) {
	r := Classify(nil, nil)
	assert.Equal(t, MappingUnknown, r.Mapping)
	assert.False(t, r.Public.IsValid())
}

func TestObservedAddressOfOurOwnInterfaceMeansNoNAT(t *testing.T) {
	local := []netip.Addr{netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("203.0.113.7")}
	r := Classify(obs("a", "203.0.113.7:4242", "b", "203.0.113.7:4242"), local)
	assert.True(t, r.NoNAT)

	behind := Classify(obs("a", "203.0.113.7:4242", "b", "203.0.113.7:4242"), []netip.Addr{netip.MustParseAddr("192.168.1.5")})
	assert.False(t, behind.NoNAT)
}

func TestIPv6AnswersDoNotCountAsDisagreement(t *testing.T) {
	r := Classify(obs("a", "203.0.113.7:50000", "b", "203.0.113.7:50000", "c", "[2001:db8::1]:50000"), nil)
	assert.Equal(t, MappingStable, r.Mapping, "a v6 answer is another network, not a different mapping")
	assert.Equal(t, ap("203.0.113.7:50000"), r.Public)

	only6 := Classify(obs("a", "[2001:db8::1]:50000", "b", "[2001:db8::1]:50000"), nil)
	assert.Equal(t, MappingStable, only6.Mapping)
}

func TestMajorityDecidesThePublicAddress(t *testing.T) {
	r := Classify(obs("a", "203.0.113.7:1111", "b", "203.0.113.7:2222", "c", "203.0.113.7:2222"), nil)
	assert.Equal(t, ap("203.0.113.7:2222"), r.Public)
	assert.Equal(t, MappingVaries, r.Mapping)
}

func TestIgnoresGarbageObservations(t *testing.T) {
	r := Classify([]Observation{{From: "a"}, {From: "b", Addr: ap("203.0.113.7:0")}}, nil)
	assert.False(t, r.Public.IsValid())
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
	s1 := fakeSTUN(t, ap("198.51.100.1:40000"))
	s2 := fakeSTUN(t, ap("198.51.100.1:40000"))
	got, err := QuerySTUN(context.Background(), clientSocket(t), []string{s1, s2}, 2*time.Second)
	require.NoError(t, err)
	require.Len(t, got, 2)
	r := Classify(got, nil)
	assert.Equal(t, MappingStable, r.Mapping)
	assert.Equal(t, ap("198.51.100.1:40000"), r.Public)
}

func TestQuerySTUNDetectsASymmetricNAT(t *testing.T) {
	s1 := fakeSTUN(t, ap("198.51.100.1:40000"))
	s2 := fakeSTUN(t, ap("198.51.100.1:40001")) // another port for another destination
	got, err := QuerySTUN(context.Background(), clientSocket(t), []string{s1, s2}, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, MappingVaries, Classify(got, nil).Mapping)
}

func TestQuerySTUNSurvivesADeadServer(t *testing.T) {
	alive := fakeSTUN(t, ap("198.51.100.1:40000"))
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
