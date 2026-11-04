package carrier

import (
	"net/netip"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

var (
	ipLocal  = netip.MustParseAddr("203.0.113.10")
	ipLocal2 = netip.MustParseAddr("203.0.113.11")
	ipSame   = netip.MustParseAddr("203.0.113.20")
	ipBorder = netip.MustParseAddr("198.51.100.20")
	ipThird  = netip.MustParseAddr("192.0.2.20")
)

func mustParams(t testing.TB, sni string) []byte {
	t.Helper()
	b, err := objects.Marshal(&pb.SuiteParams{Sni: sni})
	if err != nil {
		t.Fatalf("marshal SuiteParams: %v", err)
	}
	return b
}

func planAddr(t testing.TB, suite uint32, ip netip.Addr, port uint32, sni string) *pb.Addr {
	t.Helper()
	return &pb.Addr{Suite: suite, Host: ip.String(), Port: port, Mask: 1, Params: mustParams(t, sni)}
}

func planWS(t testing.TB, url string) *pb.Addr {
	t.Helper()
	return &pb.Addr{Suite: objects.SuiteWS2, Url: url, Mask: 1}
}

func planCard(role, flags uint32, scope objects.Scope, addrs ...*pb.Addr) *objects.Card {
	return &objects.Card{Body: &pb.NodeCard{Role: role, Flags: flags, Addrs: addrs}, Scope: scope}
}

func geo(m map[netip.Addr]string) func(netip.Addr) (string, bool) {
	return func(a netip.Addr) (string, bool) {
		c, ok := m[a]
		return c, ok
	}
}

func globalGeo() func(netip.Addr) (string, bool) {
	return geo(map[netip.Addr]string{
		ipLocal:  "RU",
		ipLocal2: "RU",
		ipSame:   "RU",
		ipBorder: "DE",
		ipThird:  "CN",
	})
}

func ruInfo() LocalInfo {
	return LocalInfo{Addrs: []netip.Addr{ipLocal}, CountryOf: globalGeo()}
}

func TestPlan(t *testing.T) {
	tc := func(t *testing.T, suite uint32, ip netip.Addr) *pb.Addr {
		return planAddr(t, suite, ip, 443, "cover.example")
	}

	cases := []struct {
		name      string
		card      *objects.Card
		info      LocalInfo
		want      []uint32
		wantPorts []uint32
		relay     bool
	}{
		{
			name: "server_across_border_camouflage_only",
			card: planCard(objects.RoleServer, 0, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder),
				tc(t, objects.SuiteReality2TCP, ipBorder),
				planWS(t, "wss://cover.example/ws")),
			info: ruInfo(),
			want: []uint32{objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2},
		},
		{
			name: "server_same_country_may_use_noise3",
			card: planCard(objects.RoleServer, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipSame)),
			info: ruInfo(),
			want: []uint32{objects.SuiteNoise3},
		},
		{
			name: "domestic_only_across_border_none",
			card: planCard(objects.RoleServer, objects.FlagDomesticOnly, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder),
				tc(t, objects.SuiteReality2TCP, ipBorder)),
			info: ruInfo(),
		},
		{
			name: "domestic_only_same_country_allowed",
			card: planCard(objects.RoleServer, objects.FlagDomesticOnly, objects.ScopePublic,
				tc(t, objects.SuiteReality2TCP, ipSame)),
			info: ruInfo(),
			want: []uint32{objects.SuiteReality2TCP},
		},
		{
			name: "noise3_peer_same_country_preferred",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteReality2QUIC, ipSame),
				tc(t, objects.SuiteNoise3, ipSame)),
			info: ruInfo(),
			want: []uint32{objects.SuiteNoise3, objects.SuiteReality2QUIC},
		},
		{
			name: "noise3_public_card_skipped",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				tc(t, objects.SuiteNoise3, ipSame)),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "noise3_across_border_skipped",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipBorder)),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "user_across_border_camouflage",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder),
				tc(t, objects.SuiteReality2TCP, ipBorder)),
			info: ruInfo(),
			want: []uint32{objects.SuiteReality2QUIC, objects.SuiteReality2TCP},
		},
		{
			name: "across_border_noise3_dropped_camouflage_kept",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipBorder),
				tc(t, objects.SuiteReality2QUIC, ipBorder)),
			info: ruInfo(),
			want: []uint32{objects.SuiteReality2QUIC},
		},
		{
			name: "reality_without_sni_skipped_ws2_allowed",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder),
				planWS(t, "wss://cover.example/ws")),
			info: ruInfo(),
			want: []uint32{objects.SuiteWS2},
		},
		{
			name: "udp_blocked_drops_udp_suites",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipSame),
				tc(t, objects.SuiteReality2QUIC, ipSame),
				tc(t, objects.SuiteReality2TCP, ipSame),
				planWS(t, "wss://cover.example/ws")),
			info: func() LocalInfo { i := ruInfo(); i.UDPBlocked = true; return i }(),
			want: []uint32{objects.SuiteReality2TCP, objects.SuiteWS2},
		},
		{
			name: "udp_blocked_without_tcp_suite_needs_relay",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder)),
			info:  func() LocalInfo { i := ruInfo(); i.UDPBlocked = true; return i }(),
			relay: true,
		},
		{
			name: "unknown_country_is_border",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipThird)),
			info:  LocalInfo{Addrs: []netip.Addr{ipLocal}, CountryOf: geo(map[netip.Addr]string{ipLocal: "RU"})},
			relay: true,
		},
		{
			name: "unknown_suites_ignored",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				&pb.Addr{Suite: 0x9999, Host: ipBorder.String(), Port: 443, Mask: 1},
				&pb.Addr{Suite: 5, Host: ipBorder.String(), Port: 443, Mask: 1},
				tc(t, objects.SuiteReality2QUIC, ipBorder)),
			info: ruInfo(),
			want: []uint32{objects.SuiteReality2QUIC},
		},
		{
			name: "preferred_order_overrides_default",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder),
				tc(t, objects.SuiteReality2TCP, ipBorder),
				planWS(t, "wss://cover.example/ws")),
			info: func() LocalInfo {
				i := ruInfo()
				i.Preferred = []uint32{objects.SuiteWS2, objects.SuiteReality2TCP, objects.SuiteReality2TCP}
				return i
			}(),
			want: []uint32{objects.SuiteWS2, objects.SuiteReality2TCP, objects.SuiteReality2QUIC},
		},
		{
			name: "one_of_several_local_addresses_matches",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipSame)),
			info: LocalInfo{Addrs: []netip.Addr{ipBorder, ipLocal}, CountryOf: globalGeo()},
			want: []uint32{objects.SuiteNoise3},
		},
		{
			name:  "empty_card_needs_relay",
			card:  planCard(objects.RoleUser, 0, objects.ScopePublic),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "empty_domestic_card_none",
			card: planCard(objects.RoleUser, objects.FlagDomesticOnly, objects.ScopePublic),
			info: ruInfo(),
		},
		{
			name: "ws2_without_url_skipped",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				&pb.Addr{Suite: objects.SuiteWS2, Mask: 1}),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "card_order_kept_within_suite",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				planAddr(t, objects.SuiteReality2QUIC, ipBorder, 8443, "a.example"),
				planAddr(t, objects.SuiteReality2QUIC, ipBorder, 9443, "b.example")),
			info:      ruInfo(),
			want:      []uint32{objects.SuiteReality2QUIC, objects.SuiteReality2QUIC},
			wantPorts: []uint32{8443, 9443},
		},
		{
			name: "no_local_addresses_means_border",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				tc(t, objects.SuiteNoise3, ipSame)),
			info:  LocalInfo{CountryOf: globalGeo()},
			relay: true,
		},
		{
			name: "dns_name_remote_is_unknown",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				&pb.Addr{Suite: objects.SuiteNoise3, Host: "peer.example", Port: 443, Mask: 1, Params: mustParams(t, "")}),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "noise3_without_port_skipped",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				planAddr(t, objects.SuiteNoise3, ipSame, 0, "")),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "reality_without_host_skipped",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				&pb.Addr{Suite: objects.SuiteReality2QUIC, Host: "", Port: 443, Mask: 1, Params: mustParams(t, "cover.example")}),
			info:  ruInfo(),
			relay: true,
		},
		{
			name: "invalid_ws2_url_is_treated_as_border",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				&pb.Addr{Suite: objects.SuiteWS2, Url: "://bad", Mask: 1}),
			info: ruInfo(),
			want: []uint32{objects.SuiteWS2},
		},
		{
			name: "nil_country_function_means_border",
			card: planCard(objects.RoleUser, 0, objects.ScopePublic,
				tc(t, objects.SuiteReality2QUIC, ipBorder)),
			info: LocalInfo{Addrs: []netip.Addr{ipLocal}},
			want: []uint32{objects.SuiteReality2QUIC},
		},
		{
			name: "noise3_without_addressing_skipped_peer_same_country",
			card: planCard(objects.RoleUser, 0, objects.ScopePeer,
				&pb.Addr{Suite: objects.SuiteNoise3, Host: "", Port: 443, Mask: 1}),
			info:  ruInfo(),
			relay: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := Plan(c.card, c.info)
			if c.relay {
				if len(got) != 1 || got[0].Kind != AttemptRelay {
					t.Fatalf("Plan() = %+v, want one relay attempt", got)
				}
				return
			}
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("Plan() = %+v, want no attempts", got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("Plan() has %d attempts, want %d: %+v", len(got), len(c.want), got)
			}
			for i, a := range got {
				if a.Kind != AttemptCarrier {
					t.Fatalf("attempt %d: kind = %v, want carrier", i, a.Kind)
				}
				if a.Suite != c.want[i] {
					t.Fatalf("attempt %d: suite = %d, want %d", i, a.Suite, c.want[i])
				}
				if a.Addr == nil {
					t.Fatalf("attempt %d: nil address", i)
				}
				if c.wantPorts != nil && a.Addr.GetPort() != c.wantPorts[i] {
					t.Fatalf("attempt %d: port = %d, want %d", i, a.Addr.GetPort(), c.wantPorts[i])
				}
			}
		})
	}
}

func TestPlanNilCard(t *testing.T) {
	if got := Plan(nil, ruInfo()); got != nil {
		t.Fatalf("Plan(nil) = %+v, want nil", got)
	}
	if got := Plan(&objects.Card{}, ruInfo()); got != nil {
		t.Fatalf("Plan(empty card) = %+v, want nil", got)
	}
}

func TestPlanDoesNotMutateCard(t *testing.T) {
	a := planAddr(t, objects.SuiteReality2TCP, ipBorder, 443, "cover.example")
	b := planAddr(t, objects.SuiteReality2QUIC, ipBorder, 443, "cover.example")
	c := planCard(objects.RoleUser, 0, objects.ScopePublic, a, b)
	got := Plan(c, ruInfo())
	if len(got) != 2 || got[0].Addr != b || got[1].Addr != a {
		t.Fatalf("Plan() = %+v, want card order reordered by suite preference", got)
	}
	if c.Body.Addrs[0] != a || c.Body.Addrs[1] != b {
		t.Fatal("Plan mutated the card's address order")
	}
}
