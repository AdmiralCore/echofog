package portmap_test

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/portmap"
)

func pcpGateway(h *harness) {
	h.on("pcp_announce_req.hex", "pcp_announce_resp.hex")
	h.on("pcp_map_req_udp.hex", "pcp_map_resp_udp.hex")
	h.on("pcp_map_req_tcp.hex", "pcp_map_resp_tcp.hex")
}

func natpmpGateway(h *harness) {
	h.on("pcp_announce_req.hex", "pcp_unsupported_resp.hex")
	h.on("natpmp_ext_req.hex", "natpmp_ext_resp.hex")
	h.on("natpmp_map_req_udp.hex", "natpmp_map_resp_udp.hex")
	h.on("natpmp_map_req_tcp.hex", "natpmp_map_resp_tcp.hex")
}

var upnpOK = map[string][]string{
	"GetExternalIPAddress": {"upnp_extip_resp.xml"},
	"AddPortMapping":       {"upnp_add_resp.xml"},
	"DeletePortMapping":    {"upnp_del_resp.xml"},
}

func TestDisabledSendsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Enabled = false
	h.upnp("upnp_desc.xml", upnpOK)
	pcpGateway(h)
	m := h.start()

	if err := m.Add(portmap.UDP, udpPort); !errors.Is(err, portmap.ErrDisabled) {
		t.Fatalf("Add = %v, want ErrDisabled", err)
	}
	m.Confirm(extAddr)
	h.clock.Advance(3 * time.Hour)
	settle()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.traffic(); n != 0 {
		t.Fatalf("%d packets reached the gateway with port_mapping=false", n)
	}
	if h.gwCalls.Load()+h.localCalls.Load()+h.ssdpCalls.Load() != 0 {
		t.Fatal("the network was probed with port_mapping=false")
	}
	if len(m.Mappings()) != 0 || len(m.Candidates()) != 0 {
		t.Fatal("mappings appeared with port_mapping=false")
	}
}

func TestPCPMapsUDPAndTCP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pcpGateway(h)
	m := h.start()
	if err := m.Add(portmap.UDP, udpPort); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(portmap.TCP, tcpPort); err != nil {
		t.Fatal(err)
	}
	h.eventually(mapped(m, 2))

	got := m.Mappings()
	u, ok := find(got, portmap.UDP, udpPort)
	if !ok || u.External != ap(40000) || u.Via != "PCP" {
		t.Fatalf("udp mapping = %+v", u)
	}
	tc, ok := find(got, portmap.TCP, tcpPort)
	if !ok || tc.External != ap(40001) || tc.Via != "PCP" {
		t.Fatalf("tcp mapping = %+v", tc)
	}
	h.noUnexpected()
	if len(h.httpLog()) != 0 || h.ssdpCalls.Load() != 0 {
		t.Fatal("UPnP was tried although PCP worked")
	}
}

func TestPCPUnsupportedFallsBackToNATPMP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	natpmpGateway(h)
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	_ = m.Add(portmap.TCP, tcpPort)
	h.eventually(mapped(m, 2))

	u, _ := find(m.Mappings(), portmap.UDP, udpPort)
	tc, _ := find(m.Mappings(), portmap.TCP, tcpPort)
	if u.External != ap(40000) || tc.External != ap(40001) || u.Via != "NAT-PMP" {
		t.Fatalf("mappings = %+v %+v", u, tc)
	}
	h.noUnexpected()
	if len(h.httpLog()) != 0 {
		t.Fatal("UPnP was tried although NAT-PMP worked")
	}
}

func TestFallsBackToUPnP(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // the gateway does not answer PCP or NAT-PMP
	h.upnp("upnp_desc.xml", upnpOK)
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, mapped(m, 1))

	u, _ := find(m.Mappings(), portmap.UDP, udpPort)
	if u.External != ap(udpPort) || u.Via != "UPnP" {
		t.Fatalf("mapping = %+v", u)
	}
	adds := h.soapBodies("AddPortMapping")
	if len(adds) != 1 {
		t.Fatalf("%d AddPortMapping calls", len(adds))
	}
	for _, want := range []string{
		"<NewExternalPort>4433</NewExternalPort>", "<NewProtocol>UDP</NewProtocol>",
		"<NewInternalPort>4433</NewInternalPort>", "<NewInternalClient>192.168.1.10</NewInternalClient>",
		"<NewPortMappingDescription>UDP 4433</NewPortMappingDescription>", "<NewLeaseDuration>3600</NewLeaseDuration>",
		"<NewEnabled>1</NewEnabled>",
	} {
		if !strings.Contains(adds[0], want) {
			t.Errorf("AddPortMapping lacks %s:\n%s", want, adds[0])
		}
	}
}

func TestUPnPConflictTriesAnotherPort(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.upnp("upnp_desc.xml", map[string][]string{
		"GetExternalIPAddress": {"upnp_extip_resp.xml"},
		"AddPortMapping":       {"upnp_err_718.xml", "upnp_add_resp.xml"},
	})
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, mapped(m, 1))

	adds := h.soapBodies("AddPortMapping")
	if len(adds) != 2 || adds[0] == adds[1] {
		t.Fatalf("AddPortMapping calls = %d, want two different ones", len(adds))
	}
	if strings.Contains(adds[1], "<NewExternalPort>4433<") {
		t.Fatal("the second attempt asked for the port that conflicted")
	}
	u, _ := find(m.Mappings(), portmap.UDP, udpPort)
	if u.External.Port() == udpPort || u.External.Port() < 1024 {
		t.Fatalf("external port = %d", u.External.Port())
	}
}

func TestUPnPOnlyPermanentLeases(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.upnp("upnp_desc.xml", map[string][]string{
		"GetExternalIPAddress": {"upnp_extip_resp.xml"},
		"AddPortMapping":       {"upnp_err_725.xml", "upnp_add_resp.xml"},
	})
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, mapped(m, 1))

	adds := h.soapBodies("AddPortMapping")
	if len(adds) != 2 || !strings.Contains(adds[1], "<NewLeaseDuration>0</NewLeaseDuration>") ||
		!strings.Contains(adds[1], "<NewExternalPort>4433<") {
		t.Fatalf("retry after error 725 = %q", adds)
	}
}

func TestDiscoveryRetriesWithBackoff(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // nothing answers
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	for i := 0; i < 60; i++ { // ten minutes of silence
		h.clock.Advance(10 * time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	settle()
	h.mu.Lock()
	dials := h.dials
	h.mu.Unlock()
	if dials < 4 || dials > 12 {
		t.Fatalf("%d discovery sockets in ten silent minutes, want a backed-off handful", dials)
	}
	if len(m.Mappings()) != 0 {
		t.Fatal("mapping without a gateway")
	}

	pcpGateway(h) // the router wakes up
	h.advanceUntil(30*time.Second, 200, mapped(m, 1))
}

func TestRenewsAtHalfTheLifetime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pcpGateway(h)
	h.on("pcp_map_req_udp_renew.hex", "pcp_map_resp_udp_renewed.hex")
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.eventually(mapped(m, 1))

	h.clock.Advance(1790 * time.Second)
	settle()
	if h.count("pcp_map_req_udp_renew.hex") != 0 {
		t.Fatal("renewed too early")
	}
	h.clock.Advance(20 * time.Second)
	h.eventually(func() bool { return h.count("pcp_map_req_udp_renew.hex") == 1 })
	h.eventually(func() bool { u, _ := find(m.Mappings(), portmap.UDP, udpPort); return u.External == ap(40500) })
	h.noUnexpected()
}

func TestRenewsAtHalfOfAShortGrant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.on("pcp_announce_req.hex", "pcp_announce_resp.hex")
	h.on("pcp_map_req_udp.hex", "pcp_map_resp_udp_short.hex")
	h.on("pcp_map_req_udp_renew.hex", "pcp_map_resp_udp_short.hex")
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.eventually(mapped(m, 1))

	h.clock.Advance(55 * time.Second) // the router granted 120 s: renewal near 60 s
	settle()
	if h.count("pcp_map_req_udp_renew.hex") != 0 {
		t.Fatal("renewed too early")
	}
	h.clock.Advance(10 * time.Second)
	h.eventually(func() bool { return h.count("pcp_map_req_udp_renew.hex") == 1 })
}

func TestRenewalIsJittered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		rand         float64
		before, past time.Duration
	}{
		{"low", 0, 1430 * time.Second, 1450 * time.Second},      // 0.8 × 1800 s
		{"high", 0.999, 2150 * time.Second, 2170 * time.Second}, // 1.2 × 1800 s
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.cfg.Rand = func() float64 { return tc.rand }
			pcpGateway(h)
			h.on("pcp_map_req_udp_renew.hex", "pcp_map_resp_udp_renewed.hex")
			m := h.start()
			_ = m.Add(portmap.UDP, udpPort)
			h.eventually(mapped(m, 1))
			h.clock.Advance(tc.before)
			settle()
			if h.count("pcp_map_req_udp_renew.hex") != 0 {
				t.Fatal("renewed before the jitter window")
			}
			h.clock.Advance(tc.past - tc.before)
			h.eventually(func() bool { return h.count("pcp_map_req_udp_renew.hex") == 1 })
		})
	}
}

func TestRemoveAndCloseUnmap(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pcpGateway(h)
	h.on("pcp_unmap_req_udp.hex", "pcp_map_resp_udp.hex")
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.eventually(mapped(m, 1))

	m.Remove(portmap.UDP, udpPort)
	h.eventually(func() bool { return h.count("pcp_unmap_req_udp.hex") == 1 })
	h.eventually(func() bool { return len(m.Mappings()) == 0 })

	_ = m.Add(portmap.UDP, udpPort)
	h.eventually(mapped(m, 1))
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.count("pcp_unmap_req_udp.hex") != 2 {
		t.Fatalf("Close left the mapping on the router (%d unmaps)", h.count("pcp_unmap_req_udp.hex"))
	}
	before := h.traffic()
	h.clock.Advance(5 * time.Hour)
	settle()
	if h.traffic() != before {
		t.Fatal("traffic after Close")
	}
	if len(m.Mappings()) != 0 {
		t.Fatal("mappings after Close")
	}
	if err := m.Add(portmap.UDP, udpPort); err == nil {
		t.Fatal("Add after Close succeeded")
	}
}

func TestUPnPUnmapOnClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.upnp("upnp_desc.xml", upnpOK)
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, mapped(m, 1))
	_ = m.Close(context.Background())
	dels := h.soapBodies("DeletePortMapping")
	if len(dels) != 1 || !strings.Contains(dels[0], "<NewExternalPort>4433</NewExternalPort>") {
		t.Fatalf("DeletePortMapping = %q", dels)
	}
}

func TestCandidatesNeedPeerConfirmation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pcpGateway(h)
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.eventually(mapped(m, 1))

	if len(m.Candidates()) != 0 {
		t.Fatal("unconfirmed external address published")
	}
	m.Confirm(netip.MustParseAddr("1.1.1.1")) // peers see another IP: double NAT or a lying router
	if len(m.Candidates()) != 0 {
		t.Fatal("candidate although peers see another IP")
	}
	before := h.changes.Load()
	m.Confirm(extAddr)
	c := m.Candidates()
	if len(c) != 1 || c[0].External != ap(40000) {
		t.Fatalf("candidates = %+v", c)
	}
	if h.changes.Load() == before {
		t.Fatal("OnChange not called when a candidate appeared")
	}
	m.Confirm(netip.Addr{}) // the address is unknown again (network change)
	if len(m.Candidates()) != 0 {
		t.Fatal("candidate survived Confirm of the zero address")
	}
}

func TestNonPublicExternalAddressIsNeverACandidate(t *testing.T) {
	t.Parallel()
	cgnat := netip.MustParseAddr("100.64.3.4")
	t.Run("pcp", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.on("pcp_announce_req.hex", "pcp_announce_resp.hex")
		h.on("pcp_map_req_udp.hex", "pcp_map_resp_udp_cgnat.hex")
		m := h.start()
		_ = m.Add(portmap.UDP, udpPort)
		h.eventually(func() bool { return h.count("pcp_map_req_udp.hex") >= 1 })
		settle()
		m.Confirm(cgnat)
		if len(m.Mappings()) != 0 || len(m.Candidates()) != 0 {
			t.Fatalf("CGNAT address exposed: %+v", m.Mappings())
		}
	})
	t.Run("natpmp", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		natpmpGateway(h)
		h.on("natpmp_ext_req.hex", "natpmp_ext_resp_cgnat.hex")
		m := h.start()
		_ = m.Add(portmap.UDP, udpPort)
		h.eventually(func() bool { return h.count("natpmp_map_req_udp.hex") >= 1 })
		settle()
		m.Confirm(cgnat)
		if len(m.Mappings()) != 0 || len(m.Candidates()) != 0 {
			t.Fatalf("CGNAT address exposed: %+v", m.Mappings())
		}
	})
	t.Run("upnp", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.upnp("upnp_desc.xml", map[string][]string{
			"GetExternalIPAddress": {"upnp_extip_resp_private.xml"},
			"AddPortMapping":       {"upnp_add_resp.xml"},
		})
		m := h.start()
		_ = m.Add(portmap.UDP, udpPort)
		h.advanceUntil(300*time.Millisecond, 100, func() bool { return len(h.soapBodies("AddPortMapping")) >= 1 })
		settle()
		m.Confirm(netip.MustParseAddr("192.168.100.2"))
		if len(m.Mappings()) != 0 || len(m.Candidates()) != 0 {
			t.Fatalf("private address exposed: %+v", m.Mappings())
		}
	})
}

func TestGatewayOutsideTheLANIsNotTalkedTo(t *testing.T) {
	t.Parallel()
	for _, gw := range []string{"8.8.8.8", "127.0.0.1", "0.0.0.0", "224.0.0.1"} {
		h := newHarness(t)
		h.cfg.Gateway = func() (netip.Addr, error) { return netip.MustParseAddr(gw), nil }
		h.upnp("upnp_desc.xml", upnpOK)
		pcpGateway(h)
		m := h.start()
		_ = m.Add(portmap.UDP, udpPort)
		h.clock.Advance(time.Minute)
		settle()
		if n := h.traffic(); n != 0 {
			t.Errorf("gateway %s: %d packets sent", gw, n)
		}
	}
}

func TestNoGatewayIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Gateway = func() (netip.Addr, error) { return netip.Addr{}, errors.New("no route") }
	m := h.start()
	if err := m.Add(portmap.UDP, udpPort); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	settle()
	if h.traffic() != 0 || len(m.Mappings()) != 0 {
		t.Fatal("traffic without a gateway")
	}
}

func TestPCPResponseForAnotherMappingIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.on("pcp_announce_req.hex", "pcp_announce_resp.hex")
	h.on("pcp_map_req_udp.hex", "pcp_map_resp_wrong_nonce.hex")
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(400*time.Millisecond, 20, func() bool { return h.count("pcp_map_req_udp.hex") >= 2 })
	if len(m.Mappings()) != 0 {
		t.Fatalf("a response with a foreign nonce created a mapping: %+v", m.Mappings())
	}
}

func TestUPnPDescriptionMustBeOnTheGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.upnp("upnp_desc.xml", upnpOK)
	h.mu.Lock()
	h.ssdp = []string{"http://192.168.1.99:5000/rootDesc.xml", "http://8.8.8.8/rootDesc.xml", "ftp://192.168.1.1/x"}
	h.mu.Unlock()
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, func() bool { return h.ssdpCalls.Load() >= 1 })
	h.clock.Advance(10 * time.Second)
	settle()
	for _, r := range h.httpLog() {
		t.Errorf("HTTP request to a location that is not the gateway: %s", r)
	}
	if len(m.Mappings()) != 0 {
		t.Fatal("mapping through a foreign UPnP device")
	}
}

func TestUPnPControlURLMustStayOnTheGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.upnp("upnp_desc_foreign_control.xml", upnpOK)
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, func() bool { return len(h.httpLog()) >= 1 })
	h.clock.Advance(10 * time.Second)
	settle()
	for _, r := range h.httpLog() {
		if strings.HasPrefix(r, "POST") {
			t.Errorf("SOAP request sent: %s", r)
		}
	}
	if len(m.Mappings()) != 0 {
		t.Fatal("mapping through a control URL on another host")
	}
}

func TestUPnPOversizedAnswerIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.upnp("upnp_desc.xml", upnpOK)
	h.mu.Lock()
	h.httpFn = func(*http.Request, []byte) (int, []byte) { return 200, make([]byte, 3<<20) }
	h.mu.Unlock()
	m := h.start()
	_ = m.Add(portmap.UDP, udpPort)
	h.advanceUntil(300*time.Millisecond, 100, func() bool { return len(h.httpLog()) >= 1 })
	settle()
	if len(m.Mappings()) != 0 {
		t.Fatal("mapping from an oversized description")
	}
}

func TestInputIsValidated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pcpGateway(h)
	m := h.start()
	if err := m.Add(portmap.UDP, 0); err == nil {
		t.Error("port 0 accepted")
	}
	if err := m.Add("icmp", 80); err == nil {
		t.Error("unknown protocol accepted")
	}
	for i := 0; i < 8; i++ {
		if err := m.Add(portmap.UDP, uint16(5000+i)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if err := m.Add(portmap.UDP, 6000); !errors.Is(err, portmap.ErrTooMany) {
		t.Fatalf("ninth port: %v, want ErrTooMany", err)
	}
	if err := m.Add(portmap.UDP, 5000); err != nil {
		t.Fatalf("adding a port twice: %v", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pcpGateway(h)
	m := h.start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = m.Add(portmap.UDP, udpPort)
				_ = m.Mappings()
				m.Confirm(extAddr)
				_ = m.Candidates()
				m.Remove(portmap.UDP, udpPort)
				h.clock.Advance(time.Second)
			}
		}()
	}
	wg.Wait()
}
