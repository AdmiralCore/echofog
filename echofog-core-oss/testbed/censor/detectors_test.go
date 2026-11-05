package censor

import (
	"math"
	"net/netip"
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
			"cover.lab":      {IPs: []netip.Addr{netip.MustParseAddr("203.0.113.15")}, ASN: "AS64500"},
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
		if m.Recall != 1 {
			t.Errorf("%s: recall %.2f, want 1 (positives=%d)", id, m.Recall, m.Positives)
		}
		if m.FPR > 0.01 {
			t.Errorf("%s: FPR %.3f above the 1%% gate (browser negatives=%d)", id, m.FPR, m.BrowserNegatives)
		}
	}
	if got["D1"].Positives != 1 || got["D10"].Positives != 1 {
		t.Errorf("control flows were not counted: D1=%d D10=%d", got["D1"].Positives, got["D10"].Positives)
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
