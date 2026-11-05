// Command coverscan measures live covers with the Chrome-shaped ClientHello (5 connections per
// cover and transport, at most one connection per second, handshake plus one HEAD) and compares
// each with the TLS server the node would run for it. It produces the table of T2.6b-1.
//
//	go run ./cmd/coverscan -sites sites.txt -json out.json -md out.md
//
// Nothing here is part of a product binary; it lives outside internal/v2 so that the mobile
// builds of internal/v2/... do not try to link it.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/profile"
)

type result struct {
	Host      string                `json:"host"`
	Transport profile.Transport     `json:"transport"`
	Cover     *profile.CoverProfile `json:"cover,omitempty"`
	Ours      *profile.CoverProfile `json:"ours,omitempty"`
	Error     string                `json:"error,omitempty"`
	OursError string                `json:"ours_error,omitempty"`
	Diffs     []profile.Diff        `json:"diffs,omitempty"`
	Observed  []profile.Diff        `json:"observed_diffs,omitempty"`
	Verdict   string                `json:"verdict"`
}

func main() {
	sitesFile := flag.String("sites", "", "file with one host per line")
	want := flag.Int("want", 20, "stop after this many hosts that serve HTTP/3")
	samples := flag.Int("samples", 5, "connections per cover and transport")
	gap := flag.Duration("gap", time.Second, "least time between two connection starts")
	jsonOut := flag.String("json", "", "write raw results here")
	mdOut := flag.String("md", "", "write the table here")
	flag.Parse()

	hosts, err := readLines(*sitesFile)
	if err != nil || len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "coverscan: need -sites:", err)
		os.Exit(2)
	}
	live := profile.NewProber(profile.ProbeConfig{Samples: *samples, Gap: *gap, Timeout: 12 * time.Second})
	local := profile.NewProber(profile.ProbeConfig{Samples: *samples, Gap: 0, NoRequest: true, Timeout: 10 * time.Second})
	ctx := context.Background()

	var results []result
	got := 0
	for _, host := range hosts {
		if got >= *want {
			break
		}
		t := profile.Target{Host: host, Addr: host + ":443", SNI: host}
		q := scan(ctx, live, local, t, profile.QUIC)
		results = append(results, q)
		fmt.Fprintf(os.Stderr, "%-28s quic %s\n", host, q.Verdict)
		if q.Cover == nil {
			continue // no HTTP/3 here: not a candidate cover (wire.md §3.4)
		}
		got++
		tc := scan(ctx, live, local, t, profile.TCP)
		results = append(results, tc)
		fmt.Fprintf(os.Stderr, "%-28s tcp  %s\n", host, tc.Verdict)
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(results, "", " ")
		_ = os.WriteFile(*jsonOut, b, 0o644)
	}
	md := render(results)
	if *mdOut != "" {
		_ = os.WriteFile(*mdOut, []byte(md), 0o644)
	} else {
		fmt.Print(md)
	}
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

func scan(ctx context.Context, live, local *profile.Prober, t profile.Target, tr profile.Transport) result {
	r := result{Host: t.Host, Transport: tr}
	var cover *profile.CoverProfile
	var err error
	if tr == profile.QUIC {
		cover, err = live.MeasureQUIC(ctx, t)
	} else {
		cover, err = live.MeasureTCP(ctx, t)
	}
	if err != nil {
		r.Error = err.Error()
		r.Verdict = "no data: " + short(err.Error())
		return r
	}
	r.Cover = cover
	if !cover.RequestOK && tr == profile.QUIC {
		r.Verdict = "no HTTP/3: " + cover.RequestInfo
		r.Cover = nil
		r.Error = "HTTP/3 request failed: " + cover.RequestInfo
		return r
	}
	cfg := profile.OwnConfig{SNI: t.SNI, Group: tls.CurveID(cover.Group), ALPN: cover.ALPN, Key: keyAlg(cover.LeafKey), SCIDLen: cover.SCIDLen}
	var srv *profile.OwnServer
	if tr == profile.QUIC {
		srv, err = profile.StartOwnQUIC(cfg)
	} else {
		srv, err = profile.StartOwnTCP(cfg)
	}
	if err != nil {
		r.OursError = err.Error()
		r.Verdict = "no: our server cannot start: " + short(err.Error())
		return r
	}
	defer srv.Close()
	lt := profile.Target{Host: t.Host, Addr: srv.Addr, SNI: t.SNI}
	var ours *profile.CoverProfile
	if tr == profile.QUIC {
		ours, err = local.MeasureQUIC(ctx, lt)
	} else {
		ours, err = local.MeasureTCP(ctx, lt)
	}
	if err != nil {
		r.OursError = err.Error()
		r.Verdict = "no: our server does not complete the handshake: " + short(err.Error())
		return r
	}
	r.Ours = ours
	r.Diffs = profile.Compare(cover, ours)
	r.Observed = profile.Compare(cover.Observable(), ours.Observable())
	r.Verdict = verdict(r)
	return r
}

func keyAlg(leaf string) x509.PublicKeyAlgorithm {
	if strings.HasPrefix(leaf, "rsa") {
		return x509.RSA
	}
	return x509.ECDSA
}

// verdict classifies what an observer can tell apart. "yes" means no observable item differs;
// "yes+pad" means only lengths differ and exact-length padding of the certificate can close them.
func verdict(r result) string {
	var bad []string
	var lenDiff *profile.Diff
	for i, d := range r.Observed {
		if d.Item == profile.ItemSCID {
			bad = append(bad, d.Item)
			continue
		}
		if d.Item == profile.ItemFlightLen {
			lenDiff = &r.Observed[i]
			continue
		}
		bad = append(bad, d.Item)
	}
	if len(bad) > 0 {
		return "no: " + strings.Join(bad, ",")
	}
	if lenDiff == nil {
		return "yes"
	}
	if r.Transport == profile.TCP {
		for _, d := range r.Diffs {
			if d.Item != profile.ItemMsgLen {
				continue
			}
			for _, s := range d.Slots {
				k := r.Cover.Inner[s].Kind
				if (k != "msg/11" && k != "msg/25") || !d.PadFixable {
					return "no: lengths beyond the certificate"
				}
			}
			return "yes+pad"
		}
		return "no: lengths"
	}
	if lenDiff.PadFixable {
		return "yes+pad?"
	}
	return "no: lengths (ours longer)"
}

func short(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

var groupNames = map[uint16]string{23: "P256", 24: "P384", 25: "P521", 29: "X25519", 4587: "P256MLKEM768", 4588: "X25519MLKEM768", 25497: "X25519Kyber768d00"}

func gname(g uint16) string {
	if n, ok := groupNames[g]; ok {
		return n
	}
	return fmt.Sprintf("0x%04x", g)
}

func cname(c uint16) string {
	return strings.TrimPrefix(tls.CipherSuiteName(c), "TLS_")
}

func render(rs []result) string {
	var b strings.Builder
	items := []string{profile.ItemVersion, profile.ItemCipher, profile.ItemGroup, profile.ItemHRR, profile.ItemALPN,
		profile.ItemExtOrder, "sid/scid/ccs", profile.ItemPacking, profile.ItemFlightLen, "tickets"}
	for _, tr := range []profile.Transport{profile.QUIC, profile.TCP} {
		fmt.Fprintf(&b, "\n### %s\n\n| host | group | cipher | ALPN | leaf key | ext order | tickets | cert msg | verdict |\n|---|---|---|---|---|---|---|---|---|\n", strings.ToUpper(string(tr)))
		var sel []result
		for _, r := range rs {
			if r.Transport == tr && r.Cover != nil {
				sel = append(sel, r)
			}
		}
		sort.Slice(sel, func(i, j int) bool { return sel[i].Host < sel[j].Host })
		for _, r := range sel {
			c := r.Cover
			cert := "-"
			if len(c.Inner) > 1 {
				cert = fmt.Sprintf("%s %d-%d", c.Inner[1].Kind, c.Inner[1].Min, c.Inner[1].Max)
			}
			unst := ""
			if len(c.Unstable) > 0 {
				unst = " unstable:" + strings.Join(c.Unstable, ",")
			}
			tk := len(c.Tickets)
			if !c.Decrypted {
				tk = len(c.Post)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %v | %d | %s | %s%s |\n", r.Host, gname(c.Group), cname(c.Cipher),
				c.ALPN, c.LeafKey, c.ExtOrder, tk, cert, r.Verdict, unst)
		}
		fmt.Fprintf(&b, "\nPer-item match of our server (`+` match, `-` differs, `~` the cover itself varies):\n\n| host | %s |\n|---|%s\n",
			strings.Join(items, " | "), strings.Repeat("---|", len(items)))
		for _, r := range sel {
			if r.Ours == nil {
				fmt.Fprintf(&b, "| %s | %s |\n", r.Host, r.Verdict)
				continue
			}
			fmt.Fprintf(&b, "| %s |", r.Host)
			for _, it := range items {
				fmt.Fprintf(&b, " %s |", mark(r.Observed, it))
			}
			b.WriteString("\n")
		}
		yes, pad, no, total := 0, 0, 0, 0
		for _, r := range sel {
			total++
			switch {
			case r.Verdict == "yes":
				yes++
			case strings.HasPrefix(r.Verdict, "yes+pad"):
				pad++
			default:
				no++
			}
		}
		fmt.Fprintf(&b, "\n%s: %d covers with data; reproduced as is: %d; reproduced with exact-length certificate padding: %d; not reproduced: %d.\n",
			strings.ToUpper(string(tr)), total, yes, pad, no)
	}
	return b.String()
}

func mark(ds []profile.Diff, item string) string {
	names := []string{item}
	switch item {
	case "sid/scid/ccs":
		names = []string{profile.ItemSessionID, profile.ItemSCID, profile.ItemCCS}
	case "tickets":
		names = []string{profile.ItemTicketCount, profile.ItemTicketLen}
	}
	m := "+"
	for _, d := range ds {
		for _, n := range names {
			if d.Item == n {
				if d.Cover == "unstable" {
					return "~"
				}
				m = "-"
			}
		}
	}
	return m
}
