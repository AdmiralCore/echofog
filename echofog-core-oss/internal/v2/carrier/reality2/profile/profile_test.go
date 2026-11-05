package profile

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"reflect"
	"testing"
	"time"
)

// fastProber measures local servers: no pacing, no HEAD request.
func fastProber() *Prober {
	return NewProber(ProbeConfig{Samples: 5, Gap: 0, Settle: 150 * time.Millisecond, NoRequest: true, Timeout: 10 * time.Second})
}

func measureOwnTCP(t *testing.T, cfg OwnConfig) *CoverProfile {
	t.Helper()
	srv, err := StartOwnTCP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	p, err := fastProber().MeasureTCP(context.Background(), Target{Host: "local", Addr: srv.Addr, SNI: cfg.SNI})
	if err != nil {
		t.Fatalf("MeasureTCP: %v", err)
	}
	return p
}

func measureOwnQUIC(t *testing.T, cfg OwnConfig) *CoverProfile {
	t.Helper()
	srv, err := StartOwnQUIC(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	p, err := fastProber().MeasureQUIC(context.Background(), Target{Host: "local", Addr: srv.Addr, SNI: cfg.SNI})
	if err != nil {
		t.Fatalf("MeasureQUIC: %v", err)
	}
	return p
}

func items(ds []Diff) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Item)
	}
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func TestParseServerHello(t *testing.T) {
	// TLS 1.3 ServerHello, X25519 key share, extension order key_share then supported_versions.
	sh := buildServerHello(false, 0x1301, 0x001d, []uint16{51, 43}, 32)
	got, err := parseServerHello(sh)
	if err != nil {
		t.Fatal(err)
	}
	want := serverHello{Version: tls.VersionTLS13, Cipher: 0x1301, Group: 0x001d, ExtOrder: []uint16{51, 43}, SessionIDLen: 32}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	hrr, err := parseServerHello(buildServerHello(true, 0x1302, 0x6399, []uint16{43, 51}, 0))
	if err != nil || !hrr.HRR || hrr.Group != 0x6399 {
		t.Fatalf("HRR: %+v %v", hrr, err)
	}
	for i := 0; i < len(sh); i++ { // every truncation is an error, never a panic
		if _, err := parseServerHello(sh[:i]); err == nil {
			t.Fatalf("truncated to %d bytes parsed", i)
		}
	}
}

func TestTCPLocalGoServer(t *testing.T) {
	p := measureOwnTCP(t, OwnConfig{SNI: "cover.test", Group: tls.X25519, ALPN: "h2", Key: x509.ECDSA})
	if p.Version != tls.VersionTLS13 || p.Group != uint16(tls.X25519) || p.ALPN != "h2" || p.HRR {
		t.Fatalf("open fields: %+v", p)
	}
	if !p.CCS || len(p.ExtOrder) != 2 || len(p.Flight) < 3 {
		t.Fatalf("shape: ccs=%v ext=%v flight=%v", p.CCS, p.ExtOrder, p.Flight)
	}
	if !p.Decrypted || len(p.Tickets) != 1 || len(p.Inner) != 4 {
		t.Fatalf("decrypted=%v tickets=%v inner=%v (want EE, Certificate, CertificateVerify, Finished and one ticket)",
			p.Decrypted, p.Tickets, p.Inner)
	}
	if p.Inner[1].Kind != "msg/11" {
		t.Fatalf("inner kinds %v", p.Inner)
	}
	if len(p.Unstable) != 0 {
		t.Fatalf("a Go server must be stable across 5 samples, unstable: %v", p.Unstable)
	}
	if p.Samples != 5 {
		t.Fatalf("samples %d", p.Samples)
	}
}

func TestQUICLocalGoServer(t *testing.T) {
	p := measureOwnQUIC(t, OwnConfig{SNI: "cover.test", Group: tls.X25519, ALPN: "h3", Key: x509.ECDSA})
	if p.Version != tls.VersionTLS13 || p.Group != uint16(tls.X25519) || p.ALPN != "h3" {
		t.Fatalf("open fields: %+v", p)
	}
	if p.SCIDLen == 0 || len(p.ExtOrder) != 2 {
		t.Fatalf("scid=%d ext=%v", p.SCIDLen, p.ExtOrder)
	}
	if len(p.Flight) < 2 || p.Flight[0].Kind != "initial" {
		t.Fatalf("flight %v", p.Flight)
	}
}

func TestCompareIdentityAndRealDivergences(t *testing.T) {
	base := OwnConfig{SNI: "cover.test", Group: tls.X25519, ALPN: "h2", Key: x509.ECDSA}
	a := measureOwnTCP(t, base)
	if d := Compare(a, a); len(d) != 0 {
		t.Fatalf("a profile compared with itself differs: %+v", d)
	}

	g := base
	g.Group = tls.X25519MLKEM768
	if d := items(Compare(measureOwnTCP(t, g), a)); !contains(d, ItemGroup) {
		t.Fatalf("group divergence not caught: %v", d)
	}

	nt := base
	nt.NoTickets = true
	if d := items(Compare(measureOwnTCP(t, nt), a)); !contains(d, ItemTicketCount) {
		t.Fatalf("tickets divergence not caught: %v", d)
	}

	rsa := base
	rsa.Key = x509.RSA
	diffs := Compare(measureOwnTCP(t, rsa), a)
	if !contains(items(diffs), ItemFlightLen) {
		t.Fatalf("certificate length divergence not caught: %+v", diffs)
	}
	for _, df := range diffs {
		if df.Item == ItemFlightLen && !df.PadFixable {
			t.Fatalf("our shorter ECDSA flight is fixable by padding: %+v", df)
		}
	}

	al := base
	al.ALPN = "http/1.1"
	if d := items(Compare(measureOwnTCP(t, al), a)); !contains(d, ItemALPN) {
		t.Fatalf("ALPN divergence not caught: %v", d)
	}
}

func TestCompareQUICDivergence(t *testing.T) {
	base := OwnConfig{SNI: "cover.test", Group: tls.X25519, ALPN: "h3", Key: x509.ECDSA}
	a := measureOwnQUIC(t, base)
	g := base
	g.Group = tls.X25519MLKEM768
	if d := items(Compare(measureOwnQUIC(t, g), a)); !contains(d, ItemGroup) {
		t.Fatalf("QUIC group divergence not caught: %v", d)
	}
}

// Items a Go server cannot vary (cipher, extension order, packing, CCS, ...) are checked on a
// measured profile with one field spoiled.
func TestCompareCatchesEachItem(t *testing.T) {
	base := measureOwnTCP(t, OwnConfig{SNI: "cover.test", Group: tls.X25519, ALPN: "h2", Key: x509.ECDSA})
	clone := func(mut func(p *CoverProfile)) *CoverProfile {
		c := *base
		c.ExtOrder = append([]uint16(nil), base.ExtOrder...)
		c.Flight = append([]Slot(nil), base.Flight...)
		c.Post = append([]Slot(nil), base.Post...)
		c.Inner = append([]Slot(nil), base.Inner...)
		c.Tickets = append([]Slot(nil), base.Tickets...)
		c.Unstable = nil
		mut(&c)
		return &c
	}
	cases := []struct {
		name string
		item string
		mut  func(p *CoverProfile)
	}{
		{"version", ItemVersion, func(p *CoverProfile) { p.Version = tls.VersionTLS12 }},
		{"cipher", ItemCipher, func(p *CoverProfile) { p.Cipher ^= 1 }},
		{"hrr", ItemHRR, func(p *CoverProfile) { p.HRR = !p.HRR }},
		{"ext order", ItemExtOrder, func(p *CoverProfile) { p.ExtOrder[0], p.ExtOrder[1] = p.ExtOrder[1], p.ExtOrder[0] }},
		{"session id", ItemSessionID, func(p *CoverProfile) { p.SessionIDLen++ }},
		{"ccs", ItemCCS, func(p *CoverProfile) { p.CCS = !p.CCS }},
		{"packing", ItemPacking, func(p *CoverProfile) { p.Flight = p.Flight[:len(p.Flight)-1] }},
		{"ticket count", ItemTicketCount, func(p *CoverProfile) { p.Tickets = append(p.Tickets, p.Tickets[0]) }},
		{"ticket length", ItemTicketLen, func(p *CoverProfile) { p.Tickets[0].Min += 500; p.Tickets[0].Max += 500 }},
		{"message kinds", ItemMsgs, func(p *CoverProfile) { p.Inner = p.Inner[:len(p.Inner)-1] }},
		{"certificate length", ItemMsgLen, func(p *CoverProfile) { p.Inner[1].Min += 60; p.Inner[1].Max += 60 }},
		{"flight length", ItemFlightLen, func(p *CoverProfile) { p.Flight[2].Min += 40; p.Flight[2].Max += 40 }},
		{"scid", ItemSCID, func(p *CoverProfile) { p.Transport = QUIC; p.SCIDLen += 4 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := items(Compare(clone(c.mut), base))
			if len(got) != 1 || got[0] != c.item {
				t.Fatalf("want exactly [%s], got %v", c.item, got)
			}
		})
	}
	t.Run("within the cover range", func(t *testing.T) {
		cover := clone(func(p *CoverProfile) { p.Flight[2].Max += 2 })
		if d := Compare(cover, base); len(d) != 0 {
			t.Fatalf("a length inside the cover's 5-sample range is a match: %+v", d)
		}
	})
	t.Run("unstable cover item", func(t *testing.T) {
		cover := clone(func(p *CoverProfile) { p.Unstable = []string{ItemExtOrder} })
		d := Compare(cover, base)
		if len(d) != 1 || d[0].Item != ItemExtOrder || d[0].Cover != "unstable" {
			t.Fatalf("an item the cover itself varies cannot be mirrored: %+v", d)
		}
	})
}

func TestAggregateUnstable(t *testing.T) {
	v := Vector{Transport: TCP, Version: tls.VersionTLS13, Cipher: 0x1301, Group: 0x1d, ExtOrder: []uint16{43, 51},
		Flight: []Item{{Kind: "rec/22", Len: 122}, {Kind: "rec/20", Len: 6}, {Kind: "rec/23", Len: 1000}},
		Post:   []Item{{Kind: "rec/23", Len: 250}}}
	w := v
	w.Cipher = 0x1302
	w.Flight = []Item{{Kind: "rec/22", Len: 122}, {Kind: "rec/20", Len: 6}, {Kind: "rec/23", Len: 1002}}
	w.Post = []Item{{Kind: "rec/23", Len: 250}, {Kind: "rec/23", Len: 250}}
	p := Aggregate([]Vector{v, w})
	if !contains(p.Unstable, ItemCipher) || !contains(p.Unstable, ItemTicketCount) {
		t.Fatalf("unstable: %v", p.Unstable)
	}
	if p.Flight[2].Min != 1000 || p.Flight[2].Max != 1002 {
		t.Fatalf("range %+v", p.Flight[2])
	}
}

type memKV map[string][]byte

func (m memKV) Get(_ context.Context, k []byte) ([]byte, bool, error) {
	v, ok := m[string(k)]
	return v, ok, nil
}
func (m memKV) Put(_ context.Context, k, v []byte) error { m[string(k)] = v; return nil }

func TestStoreRoundTrip(t *testing.T) {
	kv := memKV{}
	p := &CoverProfile{Schema: SchemaVersion, Transport: QUIC, Host: "example.test", Samples: 5, Version: tls.VersionTLS13,
		ExtOrder: []uint16{43, 51}, Flight: []Slot{{Kind: "initial", Min: 1200, Max: 1200}}}
	if err := Save(context.Background(), kv, p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(context.Background(), kv, QUIC, "example.test")
	if err != nil || !ok || !reflect.DeepEqual(got, p) {
		t.Fatalf("load: %+v %v %v", got, ok, err)
	}
	if _, ok, _ := Load(context.Background(), kv, TCP, "example.test"); ok {
		t.Fatal("tcp and quic profiles must not share a key")
	}
	for k := range kv {
		kv[k] = []byte(`{"schema":99}`)
	}
	if _, _, err := Load(context.Background(), kv, QUIC, "example.test"); err == nil {
		t.Fatal("a newer schema must be refused")
	}
}

func TestPacing(t *testing.T) {
	now := time.Unix(1000, 0)
	var slept []time.Duration
	p := NewProber(ProbeConfig{Gap: time.Second,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil }})
	_ = p.pace(context.Background())
	now = now.Add(300 * time.Millisecond)
	_ = p.pace(context.Background())
	now = now.Add(2 * time.Second)
	_ = p.pace(context.Background())
	if len(slept) != 1 || slept[0] != 700*time.Millisecond {
		t.Fatalf("connection starts must be >= 1 s apart; slept %v", slept)
	}
}

func FuzzParseServerHello(f *testing.F) {
	f.Add(buildServerHello(false, 0x1301, 0x1d, []uint16{51, 43}, 32))
	f.Add(buildServerHello(true, 0x1302, 0x6399, []uint16{43, 51}, 0))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseServerHello(b) })
}

func FuzzObserveTCP(f *testing.F) {
	sh := buildServerHello(false, 0x1301, 0x1d, []uint16{51, 43}, 32)
	rec := append([]byte{22, 3, 3, byte(len(sh) >> 8), byte(len(sh))}, sh...)
	f.Add(rec, []byte{23, 3, 3, 0, 1, 0})
	f.Fuzz(func(t *testing.T, srv, cli []byte) {
		_, _ = ObserveTCP([]Event{{Data: cli}, {FromServer: true, Data: srv}, {Data: cli}})
	})
}

func FuzzObserveQUIC(f *testing.F) {
	f.Add([]byte{0xc0, 0, 0, 0, 1, 4, 1, 2, 3, 4, 4, 5, 6, 7, 8, 0, 0x40, 0x20}, []byte{0xc0, 0, 0, 0, 1, 4, 1, 2, 3, 4, 0, 0})
	f.Fuzz(func(t *testing.T, srv, cli []byte) {
		_, _ = ObserveQUIC([]Event{{Data: cli}, {FromServer: true, Data: srv}})
	})
}
