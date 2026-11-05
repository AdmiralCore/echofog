package censor

import (
	"math"
	"net/netip"
	"strings"
	"testing"
)

func labAddr(ip string, port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr(ip), port)
}

func browserTLS() Flow {
	return Flow{
		Proto: "tcp", Client: labAddr("203.0.113.21", 40000), Server: labAddr("203.0.113.15", 443),
		Looks: "TLS", SNI: "cover.lab", PayDn: 4096,
		Hello: &clientHello{SNI: "cover.lab", Grease: true, JA3: "browser-ja3", JA4: "t13d1516h2_browser", ALPN: []string{"h2"}},
	}
}

func TestEvaluateDetectorsFindsControlsAndKeepsBrowserClean(t *testing.T) {
	cfg := DetectorConfig{
		BlockedSNI: map[string]bool{"blocked.example": true},
		Hosted:     map[netip.Addr][]string{netip.MustParseAddr("203.0.113.15"): {"cover.lab"}},
		ServerASN:  map[netip.Addr]string{netip.MustParseAddr("203.0.113.15"): "AS64500"},
		DNS: map[string]ResolvedName{
			"cover.lab":       {IPs: []netip.Addr{netip.MustParseAddr("203.0.113.15")}, ASN: "AS64500"},
			"blocked.example": {IPs: []netip.Addr{netip.MustParseAddr("198.51.100.20")}, ASN: "AS64501"},
			"foreign.example": {IPs: []netip.Addr{netip.MustParseAddr("198.51.100.7")}, ASN: "AS64501"},
		},
		BrowserJA4: map[string]bool{"t13d1516h2_browser": true},
	}
	browser := browserTLS()

	randomTCP := Flow{
		Proto: "tcp", Client: labAddr("203.0.113.21", 40001), Server: labAddr("203.0.113.15", 4343),
		Looks: "random", FET: true, FirstSize: 600,
	}
	quicBlocked := Flow{
		Proto: "udp", Client: labAddr("203.0.113.21", 40002), Server: labAddr("203.0.113.15", 4343),
		Looks: "QUIC", SNI: "blocked.example", QUICDecrypted: true, UDPFirst: 1250,
		Hello: &clientHello{SNI: "blocked.example", Grease: true, JA3: "go-ja3", JA4: "q13d1516h3_go"},
	}
	foreignTLS := Flow{
		Proto: "tcp", Client: labAddr("203.0.113.21", 40003), Server: labAddr("203.0.113.15", 4343),
		Looks: "TLS", SNI: "foreign.example", SNIForeign: true,
		Hello: &clientHello{SNI: "foreign.example", Grease: false, JA3: "go-ja3", JA4: "t13d1516h2_go"},
	}
	udpNoise := Flow{
		Proto: "udp", Client: labAddr("203.0.113.21", 40004), Server: labAddr("203.0.113.15", 4343),
		Looks: "QUIC-shaped noise", UDPFirst: 1200, UDPPackets: 25, UDPEntropy: 7.9, UDPPopAvg: 4.0,
	}
	browserQUIC := Flow{
		Proto: "udp", Client: labAddr("203.0.113.21", 40005), Server: labAddr("203.0.113.15", 4343),
		Looks: "QUIC", SNI: "cover.lab", QUICDecrypted: true, UDPFirst: 1250, UDPPackets: 12,
		Hello: &clientHello{SNI: "cover.lab", Grease: true, JA3: "quic-browser-ja3", JA4: "q13d1516h3_browser"},
	}

	flows := []LabeledFlow{
		{Flow: browser, Label: LabelBrowser, Suite: "curl-control"},
		{Flow: randomTCP, Label: LabelCarrier, Suite: "tcp-random"},
		{Flow: quicBlocked, Label: LabelTool, Suite: "quic-go"},
		{Flow: foreignTLS, Label: LabelTool, Suite: "tls-go"},
		{Flow: udpNoise, Label: LabelCarrier, Suite: "udp-noise", Synthetic: true},
		{Flow: browserQUIC, Label: LabelBrowser, Suite: "quic-chrome"},
	}
	// the evaluator must not trust CheckSNI alone: it recomputes from the tables
	cfg.BrowserJA4["q13d1516h3_browser"] = true

	got := EvaluateDetectors(flows, cfg)
	for _, id := range []string{"D1", "D2", "D3", "D4", "D10"} {
		m, ok := got[id]
		if !ok {
			t.Fatalf("%s: no metric", id)
		}
		if m.Recall == nil {
			t.Fatalf("%s: recall unavailable (positives=%d)", id, m.Positives)
		}
		if *m.Recall != 1 {
			t.Errorf("%s: recall %.2f, want 1 (positives=%d)", id, *m.Recall, m.Positives)
		}
		if m.FPR > 0.01 {
			t.Errorf("%s: FPR %.3f above the 1%% gate (browser negatives=%d)", id, m.FPR, m.BrowserNegatives)
		}
	}
	if got["D1"].Positives != 1 || got["D10"].Positives != 1 {
		t.Errorf("control flows were not counted: D1=%d D10=%d", got["D1"].Positives, got["D10"].Positives)
	}
}

// A detector that had no positive sample did not prove anything; recall must be unavailable, not 1.
func TestNoPositiveSamplesMakesRecallUnavailable(t *testing.T) {
	got := EvaluateDetectors([]LabeledFlow{
		{Flow: browserTLS(), Label: LabelBrowser, Suite: "curl-control"},
	}, DetectorConfig{})
	for _, id := range []string{"D1", "D10", "D2"} {
		m := got[id]
		if m.Positives != 0 {
			t.Fatalf("%s: positives = %d, want 0", id, m.Positives)
		}
		if m.Recall != nil {
			t.Errorf("%s: recall = %v, want unavailable (nil)", id, *m.Recall)
		}
		var note string
		for _, n := range m.Notes {
			if strings.Contains(n, "recall") || strings.Contains(n, "positive") {
				note = n
			}
		}
		if note == "" {
			t.Errorf("%s: no note explaining the missing recall", id)
		}
	}
}

// D3's truth is what the scenario labelled as its control, not the detector's own rule. A product
// flow to a lab address that really hosts cover.lab must not become a D3 positive just because the
// DNS table is shorter than the Hosted table.
func TestD3TruthComesFromTheScenarioAndHostedWins(t *testing.T) {
	addr13 := netip.MustParseAddr("203.0.113.13")
	addr15 := netip.MustParseAddr("203.0.113.15")
	cfg := DetectorConfig{
		Hosted: map[netip.Addr][]string{
			addr13: {"cover.lab"},
			addr15: {"cover.lab"},
		},
		DNS: map[string]ResolvedName{
			"cover.lab":       {IPs: []netip.Addr{addr15}, ASN: "AS64500"},
			"foreign.example": {IPs: []netip.Addr{netip.MustParseAddr("198.51.100.7")}, ASN: "AS64501"},
		},
	}
	product := LabeledFlow{
		Flow:  Flow{Proto: "tcp", Server: netip.AddrPortFrom(addr13, 4242), SNI: "cover.lab"},
		Label: LabelProduct, Suite: "v1-product",
	}
	if (sniAddressDetector{}).Truth(product, cfg) {
		t.Error("a product flow was counted as a D3 positive")
	}
	if (sniAddressDetector{}).Flag(product, cfg) {
		t.Error("D3 flagged an address that Hosted says serves cover.lab")
	}
	control := LabeledFlow{
		Flow:  Flow{Proto: "tcp", Server: netip.AddrPortFrom(addr15, 4343), SNI: "foreign.example"},
		Label: LabelTool, Suite: "tls-go",
	}
	if !(sniAddressDetector{}).Truth(control, cfg) || !(sniAddressDetector{}).Flag(control, cfg) {
		t.Error("D3 missed its tls-go control")
	}
}

// D2's truth is its quic-go control, not "any flow whose SNI is in the list".
func TestD2TruthComesFromTheControlSuite(t *testing.T) {
	cfg := DetectorConfig{BlockedSNI: map[string]bool{"blocked.example": true}}
	control := LabeledFlow{
		Flow:  Flow{Proto: "udp", SNI: "blocked.example"},
		Label: LabelTool, Suite: "quic-go",
	}
	if !(quicSNIDetector{}).Truth(control, cfg) || !(quicSNIDetector{}).Flag(control, cfg) {
		t.Error("D2 missed its quic-go control")
	}
	product := LabeledFlow{
		Flow:  Flow{Proto: "udp", SNI: "blocked.example"},
		Label: LabelProduct, Suite: "v1-product",
	}
	if (quicSNIDetector{}).Truth(product, cfg) {
		t.Error("a product flow was counted as a D2 positive")
	}
}

func TestD4PrefersJA4OverShuffledJA3(t *testing.T) {
	// uTLS Chrome shuffles extension order, so JA3 changes while JA4 stays. A browser must not be
	// flagged just because its JA3 differs from the one captured for the profile.
	fl := LabeledFlow{
		Flow:  Flow{Hello: &clientHello{Grease: true, JA4: "t13d1516h2_browser", JA3: "shuffled"}},
		Label: LabelBrowser, Suite: "utls-chrome",
	}
	cfg := DetectorConfig{
		BrowserJA4: map[string]bool{"t13d1516h2_browser": true},
		BrowserJA3: map[string]bool{"other-order": true},
	}
	if (fingerprintDetector{}).Flag(fl, cfg) {
		t.Error("D4 flagged a browser whose JA4 matches")
	}
	fl.Hello.JA4 = "t13d1516h2_unknown"
	if !(fingerprintDetector{}).Flag(fl, cfg) {
		t.Error("D4 did not flag an unknown JA4")
	}
}

func TestD10DoesNotFlagRealQUIC(t *testing.T) {
	real := Flow{Proto: "udp", Looks: "QUIC", QUICDecrypted: true, UDPFirst: 1250, UDPPackets: 40, UDPEntropy: 7.9}
	noise := Flow{Proto: "udp", Looks: "QUIC-shaped noise", UDPFirst: 1200, UDPPackets: 40, UDPEntropy: 7.9}
	if (udpShapeDetector{}).Flag(LabeledFlow{Flow: real, Suite: "quic-chrome"}, DetectorConfig{}) {
		t.Error("real QUIC was flagged by D10")
	}
	if !(udpShapeDetector{}).Flag(LabeledFlow{Flow: noise, Suite: "udp-noise"}, DetectorConfig{}) {
		t.Error("QUIC-shaped noise was not flagged by D10")
	}
}

func TestEntropyAndPopcount(t *testing.T) {
	zeros := make([]byte, 100)
	if e := shannon(zeros); e != 0 {
		t.Errorf("entropy of zeros = %f, want 0", e)
	}
	if p := popAvg(zeros); p != 0 {
		t.Errorf("popcount of zeros = %f, want 0", p)
	}
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	if e := shannon(all); math.Abs(e-8) > 0.001 {
		t.Errorf("entropy of 0..255 = %f, want 8", e)
	}
}
