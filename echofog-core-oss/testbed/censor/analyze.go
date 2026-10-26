package censor

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Flow is one connection as a censor at the border sees it.
type Flow struct {
	Proto    string // "tcp" or "udp"
	Client   netip.AddrPort
	Server   netip.AddrPort
	Start    time.Time
	Duration time.Duration
	Packets  int
	BytesUp  int // client to server, IP bytes
	BytesDn  int

	// What the wire shows.
	Looks  string // "TLS", "QUIC", "QUIC-shaped noise", "HTTP", "random", "other"
	SNI    string
	Hello  *clientHello
	FET    bool   // the first data would be flagged as fully encrypted
	FETWhy string // why it is flagged or exempt

	// Shape: share of client packets of each size class, the most common payload size.
	SizeClasses [4]int // <=200, <=600, <=1200, >1200 bytes of payload, client side
	TopSize     int

	// SNIForeign: the SNI names a site the server's address does not host. A censor resolves the
	// name and finds other addresses; set by CheckSNI.
	SNIForeign bool

	// LongHeavy: one connection that lasted long and carried a lot, which an ordinary visit to a
	// site rarely does however well each packet looks; set by MarkLongHeavy. It is about the shape
	// of the flow, so it is shown apart from the verdict on what the packets look like.
	LongHeavy bool
}

// MarkLongHeavy marks the flows that lasted at least minDur and carried at least minBytes.
func MarkLongHeavy(flows []Flow, minDur time.Duration, minBytes int) {
	for i := range flows {
		f := &flows[i]
		f.LongHeavy = f.Duration >= minDur && f.BytesUp+f.BytesDn >= minBytes
	}
}

// CheckSNI marks flows whose SNI is not one the server's address really hosts. hosted lists, per
// server address, the names it serves (what DNS would answer for them).
func CheckSNI(flows []Flow, hosted map[netip.Addr][]string) {
	for i := range flows {
		f := &flows[i]
		if f.SNI == "" {
			continue
		}
		f.SNIForeign = true
		for _, n := range hosted[f.Server.Addr()] {
			if n == f.SNI {
				f.SNIForeign = false
			}
		}
	}
}

// Verdict is what a censor working on the published rules could do with the flow.
func (f Flow) Verdict() string {
	switch {
	case f.Looks == "no data":
		return "no data sent"
	case f.FET:
		return "AT RISK: looks fully encrypted (GFW blocks such flows)"
	case f.SNIForeign:
		return "AT RISK: the SNI names a site this address does not host"
	case f.Looks == "QUIC-shaped noise":
		return "AT RISK: QUIC header, but the Initial does not decrypt"
	case f.Looks == "QUIC" && f.SNI == "":
		return "suspicious: QUIC without SNI"
	case (f.Looks == "TLS" || f.Looks == "QUIC") && f.Hello != nil && !f.Hello.Grease:
		return "suspicious: TLS fingerprint of a library, not a browser (no GREASE)"
	case f.Looks == "other":
		return "unknown protocol"
	}
	return "blends in"
}

type flowKey struct {
	proto    uint8
	cli, srv netip.AddrPort
}

// Analyze reads a capture and returns its flows, the busiest first. A TCP flow's client is the
// side that sent the SYN; a UDP flow's, the side that sent first.
func Analyze(pcapPath string) ([]Flow, error) {
	pkts, err := readPcap(pcapPath)
	if err != nil {
		return nil, err
	}
	type state struct {
		f        *Flow
		upStream []byte // first client bytes of a TCP flow
		firstUp  []byte
		quic     quicInitial
		sizes    map[int]int
	}
	flows := map[flowKey]*state{}
	var order []flowKey
	for _, p := range pkts {
		k := flowKey{p.proto, p.src, p.dst}
		up := true
		st, ok := flows[k]
		if !ok {
			if rev, ok2 := flows[flowKey{p.proto, p.dst, p.src}]; ok2 {
				st, up = rev, false
			}
		}
		if st == nil {
			if p.proto == 6 && !p.syn {
				continue // joined mid-connection: the client is unknown
			}
			proto := "udp"
			if p.proto == 6 {
				proto = "tcp"
			}
			st = &state{f: &Flow{Proto: proto, Client: p.src, Server: p.dst, Start: p.at}, sizes: map[int]int{}}
			flows[k] = st
			order = append(order, k)
		}
		f := st.f
		f.Packets++
		f.Duration = p.at.Sub(f.Start)
		if !up {
			f.BytesDn += p.size
			continue
		}
		f.BytesUp += p.size
		if len(p.payload) == 0 {
			continue
		}
		n := len(p.payload)
		switch {
		case n <= 200:
			f.SizeClasses[0]++
		case n <= 600:
			f.SizeClasses[1]++
		case n <= 1200:
			f.SizeClasses[2]++
		default:
			f.SizeClasses[3]++
		}
		st.sizes[n]++
		if st.firstUp == nil {
			st.firstUp = append([]byte(nil), p.payload...)
		}
		if p.proto == 6 && len(st.upStream) < 16<<10 {
			st.upStream = append(st.upStream, p.payload...)
		}
		if p.proto == 17 && (st.quic.crypto == nil || f.Packets <= 6) {
			q := openInitials(p.payload)
			if st.quic.crypto == nil {
				st.quic = q
			} else if q.decrypted {
				st.quic.decrypted = true
				for o, d := range q.crypto {
					st.quic.crypto[o] = d
				}
			}
		}
	}

	out := make([]Flow, 0, len(order))
	for _, k := range order {
		st := flows[k]
		f := st.f
		f.FET, f.FETWhy = fullyEncrypted(st.firstUp)
		top, topN := 0, 0
		for s, n := range st.sizes {
			if n > topN || (n == topN && s < top) {
				top, topN = s, n
			}
		}
		f.TopSize = top
		switch {
		case len(st.firstUp) == 0:
			f.Looks, f.FET = "no data", false
		case f.Proto == "tcp" && startsHTTP(st.upStream):
			f.Looks = "HTTP"
		case f.Proto == "tcp":
			if hs, ok := tlsHandshake(st.upStream); ok {
				f.Looks = "TLS"
				if ch, ok := parseClientHello(hs, false); ok {
					f.Hello, f.SNI = &ch, ch.SNI
				}
			} else if f.FET {
				f.Looks = "random"
			} else {
				f.Looks = "other"
			}
		case st.quic.decrypted:
			f.Looks = "QUIC"
			f.FET, f.FETWhy = false, "real QUIC"
			if ch, ok := parseClientHello(assembleCrypto(st.quic.crypto), true); ok {
				f.Hello, f.SNI = &ch, ch.SNI
			}
		case st.quic.isLongHeader && st.quic.version == quicV1:
			f.Looks = "QUIC-shaped noise"
		case f.FET:
			f.Looks = "random"
		default:
			f.Looks = "other"
		}
		out = append(out, *f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BytesUp+out[i].BytesDn > out[j].BytesUp+out[j].BytesDn })
	return out, nil
}

// Report renders flows as a table; label names the server addresses ("x1", "web", ...).
func Report(flows []Flow, label func(netip.AddrPort) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-5s %-24s %-14s %8s %9s %-10s %-18s %-22s %s\n", "proto", "server", "looks like", "duration", "bytes", "shape", "SNI", "fingerprint", "verdict")
	for _, f := range flows {
		fp := "-"
		if f.Hello != nil {
			fp = f.Hello.Print
		}
		srv := f.Server.String()
		if l := label(f.Server); l != "" {
			srv = l + " " + f.Server.String()
		}
		sni := f.SNI
		if sni == "" {
			sni = "-"
		}
		shape := "-"
		if f.LongHeavy {
			shape = "LONG+HEAVY"
		}
		fmt.Fprintf(&b, "%-5s %-24.24s %-14.14s %8s %9s %-10s %-18.18s %-22.22s %s\n", f.Proto, srv, f.Looks,
			f.Duration.Round(time.Second), human(f.BytesUp+f.BytesDn), shape, sni, fp, f.Verdict())
	}
	return b.String()
}

func human(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
