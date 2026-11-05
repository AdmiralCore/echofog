package censor

import (
	"net/netip"
)

// A label is what the scenario knows about a flow. A detector never sees it: the label is only
// used after the fact, to count recall and the false-positive rate.
type Label string

const (
	LabelCarrier Label = "carrier" // a v2 carrier (noise3/reality2/ws2) or its harness stand-in
	LabelProduct Label = "product" // the v1 product, until efv2 exists (T0.3)
	LabelService Label = "service" // chat/file/call/exit traffic
	LabelBrowser Label = "browser" // browser control in the same network
	LabelTool    Label = "tool"    // curl/crypto/tls/quic-go: honest controls, not browsers
	LabelLab     Label = "lab"     // the test bed's own chatter, DNS, bootstrap
)

// LabeledFlow is one censor flow with the ground truth the scenario recorded for it.
type LabeledFlow struct {
	Flow
	Label     Label           `json:"label"`
	Suite     string          `json:"suite,omitempty"`
	Service   string          `json:"service,omitempty"`
	Synthetic bool            `json:"synthetic,omitempty"`
	Flags     map[string]bool `json:"flags,omitempty"` // per-detector verdicts, filled by EvaluateDetectors
}

// ResolvedName is what DNS says about an SNI: the addresses and, when known, the ASN.
type ResolvedName struct {
	IPs []netip.Addr `json:"ips,omitempty"`
	ASN string       `json:"asn,omitempty"`
}

// DetectorConfig is the offline knowledge a censor has: block lists, what each address hosts, DNS
// and the browser fingerprints collected by testbed/browserprofiles.
type DetectorConfig struct {
	BlockedSNI map[string]bool         `json:"blocked_sni,omitempty"`
	Hosted     map[netip.Addr][]string `json:"hosted,omitempty"`
	ServerASN  map[netip.Addr]string   `json:"server_asn,omitempty"`
	DNS        map[string]ResolvedName `json:"dns,omitempty"`
	BrowserJA4 map[string]bool         `json:"browser_ja4,omitempty"`
	BrowserJA3 map[string]bool         `json:"browser_ja3,omitempty"`
}

// DetectorMetric is one row of report.json: how a detector did on the labelled flows.
type DetectorMetric struct {
	Recall           float64  `json:"recall"`
	FPR              float64  `json:"fpr"`
	N                int      `json:"n"`
	Positives        int      `json:"positives"`
	Negatives        int      `json:"negatives"`
	BrowserNegatives int      `json:"browser_negatives"`
	BrowserFP        int      `json:"browser_false_positives"`
	Notes            []string `json:"notes,omitempty"`
}

// A Detector is one published censor rule. Applies says whether the flow is in its scope; Truth is
// the ground truth the scenario recorded for it; Flag is what the detector alone concludes.
type Detector interface {
	ID() string
	Name() string
	Applies(LabeledFlow) bool
	Truth(LabeledFlow, DetectorConfig) bool
	Flag(LabeledFlow, DetectorConfig) bool
}

// Detectors returns D1–D4 and D10, the passive detectors of the M1 launchpad.
func Detectors() []Detector {
	return []Detector{fetDetector{}, quicSNIDetector{}, sniAddressDetector{}, fingerprintDetector{}, udpShapeDetector{}}
}

// EvaluateDetectors runs every detector over the labelled flows and counts recall on the positives
// and the false-positive rate on the browser control only (a censor must not block ordinary people).
func EvaluateDetectors(flows []LabeledFlow, cfg DetectorConfig) map[string]DetectorMetric {
	for i := range flows {
		if flows[i].Flags == nil {
			flows[i].Flags = map[string]bool{}
		}
	}
	out := map[string]DetectorMetric{}
	for _, d := range Detectors() {
		m := DetectorMetric{}
		for i := range flows {
			fl := flows[i]
			if !d.Applies(fl) {
				continue
			}
			truth := d.Truth(fl, cfg)
			flag := d.Flag(fl, cfg)
			flows[i].Flags[d.ID()] = flag
			if truth {
				m.Positives++
				if flag {
					m.Recall++
				}
				continue
			}
			m.Negatives++
			if fl.Label == LabelBrowser {
				m.BrowserNegatives++
				if flag {
					m.BrowserFP++
				}
			}
		}
		if m.Positives > 0 {
			m.Recall /= float64(m.Positives)
		} else {
			m.Recall = 1
			m.Notes = append(m.Notes, "no positive samples in this run")
		}
		if m.BrowserNegatives > 0 {
			m.FPR = float64(m.BrowserFP) / float64(m.BrowserNegatives)
		} else {
			m.Notes = append(m.Notes, "no browser-control flows in this run")
		}
		m.N = m.Positives + m.Negatives
		out[d.ID()] = m
	}
	return out
}

// ---------------------------------------------------------------------------- D1

type fetDetector struct{}

func (fetDetector) ID() string   { return "D1" }
func (fetDetector) Name() string { return "GFW fully-encrypted first payload (FET)" }
func (fetDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "tcp"
}
func (fetDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == "tcp-random" || (f.Synthetic && f.Label == LabelCarrier && f.Proto == "tcp")
}
func (fetDetector) Flag(f LabeledFlow, _ DetectorConfig) bool { return f.FET }

// ---------------------------------------------------------------------------- D2

type quicSNIDetector struct{}

func (quicSNIDetector) ID() string   { return "D2" }
func (quicSNIDetector) Name() string { return "QUIC Initial SNI against the block list" }
func (quicSNIDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "udp" && f.SNI != ""
}
func (quicSNIDetector) Truth(f LabeledFlow, cfg DetectorConfig) bool {
	return cfg.BlockedSNI[f.SNI]
}
func (quicSNIDetector) Flag(f LabeledFlow, cfg DetectorConfig) bool {
	return cfg.BlockedSNI[f.SNI]
}

// ---------------------------------------------------------------------------- D3

type sniAddressDetector struct{}

func (sniAddressDetector) ID() string   { return "D3" }
func (sniAddressDetector) Name() string { return "SNI against the address and ASN it resolves to" }
func (sniAddressDetector) Applies(f LabeledFlow) bool {
	return f.SNI != ""
}
func (sniAddressDetector) Truth(f LabeledFlow, cfg DetectorConfig) bool {
	return foreign(f.Flow, cfg)
}
func (sniAddressDetector) Flag(f LabeledFlow, cfg DetectorConfig) bool {
	return foreign(f.Flow, cfg)
}

// foreign says the SNI names a site the server's address does not host, or resolves to another
// address/ASN. CheckSNI fills SNIForeign from Hosted; DNS and ServerASN add the ASN view.
func foreign(f Flow, cfg DetectorConfig) bool {
	if f.SNI == "" {
		return false
	}
	if f.SNIForeign {
		return true
	}
	r, ok := cfg.DNS[f.SNI]
	if !ok {
		return false
	}
	if len(r.IPs) > 0 {
		hosted := false
		for _, ip := range r.IPs {
			if ip == f.Server.Addr() {
				hosted = true
				break
			}
		}
		if !hosted {
			return true
		}
	}
	if r.ASN != "" && cfg.ServerASN[f.Server.Addr()] != "" && r.ASN != cfg.ServerASN[f.Server.Addr()] {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------- D4

type fingerprintDetector struct{}

func (fingerprintDetector) ID() string   { return "D4" }
func (fingerprintDetector) Name() string { return "JA3/JA4 and ClientHello shape against Chrome" }
func (fingerprintDetector) Applies(f LabeledFlow) bool {
	return f.Hello != nil
}
func (fingerprintDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Label == LabelTool
}
func (fingerprintDetector) Flag(f LabeledFlow, cfg DetectorConfig) bool {
	h := f.Hello
	if h == nil {
		return false
	}
	if !h.Grease {
		return true
	}
	if (len(cfg.BrowserJA4) > 0 && h.JA4 != "" && !cfg.BrowserJA4[h.JA4]) ||
		(len(cfg.BrowserJA3) > 0 && h.JA3 != "" && !cfg.BrowserJA3[h.JA3]) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------- D10

type udpShapeDetector struct{}

func (udpShapeDetector) ID() string { return "D10" }
func (udpShapeDetector) Name() string {
	return "UDP shape (QUIC v1 fingerprint, noise, 25-packet count)"
}
func (udpShapeDetector) Applies(f LabeledFlow) bool {
	return f.Proto == "udp"
}
func (udpShapeDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	return f.Suite == "udp-noise" || (f.Synthetic && f.Label == LabelCarrier)
}
func (udpShapeDetector) Flag(f LabeledFlow, _ DetectorConfig) bool {
	return udpShapeFlag(f.Flow)
}

func udpShapeFlag(f Flow) bool {
	if f.Proto != "udp" {
		return false
	}
	if f.Looks == "QUIC-shaped noise" {
		return true
	}
	// a long random UDP stream that does not open as a real QUIC Initial
	return f.UDPFirst >= 1001 && f.UDPPackets >= 25 && f.UDPEntropy >= 7.0 && !f.QUICDecrypted
}
