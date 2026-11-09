package censor

import (
	"net/netip"
	"time"
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

	id int // position in the run (1-based), set by EvaluateDetectors for detectors that look across flows
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

	// D7: the browser control's record sizes (bytes, first 16 KB) and the KS distance a flow may
	// have to it. Zero means "calibrate from the browser flows of the run".
	KSThreshold float64 `json:"ks_threshold,omitempty"`

	// D8: a flow that lasted LongMin and carried LongBytes is long and heavy; MaxConnsPerIP
	// connections to one address inside ConnWindow are a storm.
	LongMin       time.Duration `json:"long_min,omitempty"`
	LongBytes     int           `json:"long_bytes,omitempty"`
	MaxConnsPerIP int           `json:"max_conns_per_ip,omitempty"`
	ConnWindow    time.Duration `json:"conn_window,omitempty"`

	// D9: the seed of the forest.
	Seed int64 `json:"seed,omitempty"`
}

// DetectorMetric is one row of report.json: how a detector did on the labelled flows.
// Recall is nil when the run had no positive sample: "no control flows" is not "recall 1".
type DetectorMetric struct {
	Recall           *float64 `json:"recall"`
	FPR              float64  `json:"fpr"`
	N                int      `json:"n"`
	Positives        int      `json:"positives"`
	Negatives        int      `json:"negatives"`
	BrowserNegatives int      `json:"browser_negatives"`
	BrowserFP        int      `json:"browser_false_positives"`
	Notes            []string `json:"notes,omitempty"`

	// For scoring detectors (D6, D9): AUC and the share of positives found at 1% FPR.
	AUC    *float64 `json:"auc,omitempty"`
	TPRAt1 *float64 `json:"tpr_at_1pct_fpr,omitempty"`
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

// Detectors returns the passive detectors D1–D10 of the launchpad.
func Detectors() []Detector {
	return []Detector{fetDetector{}, quicSNIDetector{}, sniAddressDetector{}, fingerprintDetector{},
		tlsInTLSDetector{}, &crossLayerRTTDetector{}, &sizeTimingDetector{}, &longHeavyDetector{}, &flowPhysicsDetector{},
		udpShapeDetector{}}
}

// EvaluateDetectors runs every detector over the labelled flows and counts recall on the positives
// and the false-positive rate on the browser control only (a censor must not block ordinary people).
func EvaluateDetectors(flows []LabeledFlow, cfg DetectorConfig) map[string]DetectorMetric {
	for i := range flows {
		flows[i].Flags = map[string]bool{}
		flows[i].id = i + 1
	}
	out := map[string]DetectorMetric{}
	for _, d := range Detectors() {
		m := DetectorMetric{}
		recalled := 0
		if c, ok := d.(Contextual); ok {
			c.Begin(flows, cfg)
		}
		sc, scores := d.(Scorer)
		var posScores, negScores []float64
		for i := range flows {
			fl := flows[i]
			if !d.Applies(fl) {
				continue
			}
			truth := d.Truth(fl, cfg)
			flag := d.Flag(fl, cfg)
			flows[i].Flags[d.ID()] = flag
			if scores {
				if s := sc.Score(fl, cfg); truth {
					posScores = append(posScores, s)
				} else if fl.Label == LabelBrowser {
					negScores = append(negScores, s)
				}
			}
			if truth {
				m.Positives++
				if flag {
					recalled++
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
		if scores && len(posScores) > 0 && len(negScores) > 0 {
			auc, tpr := AUC(posScores, negScores), TPRAtFPR(posScores, negScores, 0.01)
			m.AUC, m.TPRAt1 = &auc, &tpr
		}
		if n, ok := d.(Noter); ok {
			m.Notes = append(m.Notes, n.Notes()...)
		}
		if m.Positives > 0 {
			v := float64(recalled) / float64(m.Positives)
			m.Recall = &v
		} else {
			m.Recall = nil
			m.Notes = append(m.Notes, "recall unavailable: no positive samples in this run")
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
	return f.Suite == "tcp-random" || (f.Synthetic && f.Label == LabelCarrier && f.Proto == "tcp" && !isTunnelControl(f))
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
	// The scenario's control is the quic-go flow with a blocked SNI. The detector's own list is
	// not the truth, otherwise a parser regression would silently shrink the positive set.
	return f.Suite == "quic-go" && cfg.BlockedSNI[f.SNI]
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
func (sniAddressDetector) Truth(f LabeledFlow, _ DetectorConfig) bool {
	// The scenario's controls are tls-go (foreign.example) and quic-go (blocked.example). Truth
	// comes from the labels, not from foreign(): the rule must not grade itself.
	return f.SNI != "" && (f.Suite == "tls-go" || f.Suite == "quic-go")
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
	// Hosted is what CheckSNI uses and what the address really serves. If it serves the name,
	// the flow is not foreign even when the DNS table is shorter than the Hosted table.
	if names, ok := cfg.Hosted[f.Server.Addr()]; ok {
		for _, n := range names {
			if n == f.SNI {
				return false
			}
		}
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
	// JA4 is order-independent (uTLS Chrome shuffles extensions), so it is the primary label;
	// JA3 is the fallback only when no JA4 could be computed.
	if len(cfg.BrowserJA4) > 0 && h.JA4 != "" {
		return !cfg.BrowserJA4[h.JA4]
	}
	if len(cfg.BrowserJA3) > 0 && h.JA3 != "" {
		return !cfg.BrowserJA3[h.JA3]
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
