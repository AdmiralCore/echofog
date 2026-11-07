package routepolicy_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	rp "github.com/echofog/echofog-core/internal/v2/routepolicy"
)

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignedRulesRoundTrip(t *testing.T) {
	pub, priv := keys(t)
	body := `{"seq":7,"version":"2026-11-06","allowed_cidrs":["198.51.100.0/24"],"mobile_asn":[8359],"neighbors":{"RU":["KZ","BY"]}}`
	r, err := rp.Parse(rp.Sign(priv, []byte(body)), pub, 3)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Seq != 7 || r.Version != "2026-11-06" {
		t.Fatalf("seq %d version %q", r.Seq, r.Version)
	}
	if !r.HasVerifiedEntries() || !r.EntryAllowed("RU", rp.NetMobile, netip.MustParseAddr("198.51.100.77")) {
		t.Fatal("an address inside allowed_cidrs is a verified entry")
	}
	if r.EntryAllowed("RU", rp.NetMobile, netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("an address outside allowed_cidrs is not")
	}
	if !r.MobileASN(8359) || r.MobileASN(1) {
		t.Fatal("mobile ASN list")
	}
	// A listed neighbour is data for the report; it decides like a foreign country (no measurement yet).
	if r.Classify("RU", "KZ") != rp.DestNeighbor {
		t.Fatal("KZ is a listed neighbour")
	}
	d := r.Decide("RU", rp.NetMobile, rp.DestNeighbor)
	if d.Direct || !d.EntryInCountry {
		t.Fatalf("a neighbour decides like a foreign country: %+v", d)
	}
}

func TestRulesRefusals(t *testing.T) {
	pub, priv := keys(t)
	otherPub, _ := keys(t)
	good := rp.Sign(priv, []byte(`{"seq":5,"version":"v"}`))
	flip := append([]byte(nil), good...)
	if len(flip) > 2 {
		flip[len(flip)-2] ^= 1
	}
	manyRanges := `{"seq":9,"allowed_cidrs":[` + strings.Repeat(`"198.51.100.0/24",`, 300) + `"198.51.100.0/24"]}`
	cases := []struct {
		name string
		data []byte
		pub  ed25519.PublicKey
		min  uint64
	}{
		{"wrong key", good, otherPub, 0},
		{"tampered body", flip, pub, 0},
		{"older than the last seen", good, pub, 6},
		{"empty", nil, pub, 0},
		{"short", make([]byte, 10), pub, 0},
		{"too large", rp.Sign(priv, []byte(`{"seq":9,"version":"`+strings.Repeat("x", 70000)+`"}`)), pub, 0},
		{"not json", rp.Sign(priv, []byte(`seq=9`)), pub, 0},
		{"bad prefix", rp.Sign(priv, []byte(`{"seq":9,"allowed_cidrs":["300.1.1.0/24"]}`)), pub, 0},
		{"bad country", rp.Sign(priv, []byte(`{"seq":9,"neighbors":{"russia":["KZ"]}}`)), pub, 0},
		{"too many ranges", rp.Sign(priv, []byte(manyRanges)), pub, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := rp.Parse(c.data, c.pub, c.min); !errors.Is(err, rp.ErrRules) {
				t.Fatalf("want ErrRules, got %v", err)
			}
		})
	}
	if _, err := rp.Parse(good, pub, 5); err != nil {
		t.Fatalf("the same seq is accepted again (reload): %v", err)
	}
}

// AD18: the file can only tighten the built-in table.
func TestRulesOnlyTighten(t *testing.T) {
	pub, priv := keys(t)
	body := `{"seq":1,"tcp_limit":4096,"udp_denied":["CN","DE"]}`
	r, err := rp.Parse(rp.Sign(priv, []byte(body)), pub, 0)
	if err != nil {
		t.Fatal(err)
	}
	d := r.Decide("RU", rp.NetMobile, rp.DestForeign)
	if d.TCPLimit != 4096 || d.Probe != 4096 {
		t.Fatalf("a lower limit applies to the probe too: %+v", d)
	}
	// An unlimited row stays unlimited by the table's own rule; the file's limit may add one.
	if d := r.Decide("DE", rp.NetWired, rp.DestForeign); d.UDP || d.TCPLimit != 4096 {
		t.Fatalf("udp_denied and tcp_limit tighten the row: %+v", d)
	}
	if d := r.Decide("CN", rp.NetWired, rp.DestForeign); d.UDP || !d.AllowsSuite(rt) {
		t.Fatalf("CN: udp denied but TCP suites stay: %+v", d)
	}
	// A limit above the table's 12 KiB cannot loosen it.
	loose, err := rp.Parse(rp.Sign(priv, []byte(`{"seq":2,"tcp_limit":1000000}`)), pub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d := loose.Decide("RU", rp.NetMobile, rp.DestForeign); d.TCPLimit != 12288 || d.Direct {
		t.Fatalf("the file loosened the table: %+v", d)
	}
	// Domestic traffic is not limited by a foreign-DC limit.
	if d := r.Decide("RU", rp.NetMobile, rp.DestDomestic); d.TCPLimit != 0 {
		t.Fatalf("domestic row got a limit: %+v", d)
	}
}

func FuzzParseRules(f *testing.F) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.Add(rp.Sign(priv, []byte(`{"seq":1,"version":"v","allowed_cidrs":["198.51.100.0/24"],"neighbors":{"RU":["KZ"]},"tcp_limit":1}`)))
	f.Add([]byte{})
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := rp.Parse(data, pub, 0)
		if err == nil {
			// A file that parses can be used.
			_ = r.Decide("RU", rp.NetMobile, rp.DestForeign)
			_ = r.EntryAllowed("RU", rp.NetMobile, netip.MustParseAddr("198.51.100.1"))
		} else if !errors.Is(err, rp.ErrRules) {
			t.Fatalf("a refusal must wrap ErrRules: %v", err)
		}
	})
}

// ---- T12.5: valid_until (H3a)

type clockAt struct{ t time.Time }

func (c *clockAt) now() time.Time { return c.t }

var rulesNow = time.Unix(1_800_000_000, 0)

func fileBody(seq uint64, validUntil time.Time, extra string) []byte {
	b := fmt.Sprintf(`{"seq":%d,"version":"2026-11","allowed_cidrs":["198.51.100.0/24"],"tcp_limit":20000,"udp_denied":["RU"]%s`, seq, extra)
	if !validUntil.IsZero() {
		b += fmt.Sprintf(`,"valid_until":%d`, validUntil.Unix())
	}
	return []byte(b + "}")
}

func TestParseAtNeedsValidUntil(t *testing.T) {
	pub, priv := keys(t)
	clk := &clockAt{rulesNow}
	if _, err := rp.ParseAt(rp.Sign(priv, fileBody(1, time.Time{}, "")), pub, 0, clk.now); !errors.Is(err, rp.ErrRules) {
		t.Fatalf("a file without valid_until: %v", err)
	}
	r, err := rp.ParseAt(rp.Sign(priv, fileBody(1, rulesNow.Add(30*24*time.Hour), "")), pub, 0, clk.now)
	if err != nil {
		t.Fatalf("ParseAt: %v", err)
	}
	if !r.ValidUntil().Equal(rulesNow.Add(30*24*time.Hour)) || r.Expired() {
		t.Fatalf("valid until %v, expired %v", r.ValidUntil(), r.Expired())
	}
	for name, vu := range map[string]time.Time{
		"too far ahead":   rulesNow.Add(rp.MaxValidity + time.Hour),
		"a century ahead": rulesNow.Add(100 * 365 * 24 * time.Hour),
		"zero":            time.Unix(0, 0),
		"negative":        time.Unix(-5, 0),
	} {
		body := fmt.Sprintf(`{"seq":2,"valid_until":%d}`, vu.Unix())
		if _, err := rp.ParseAt(rp.Sign(priv, []byte(body)), pub, 0, clk.now); !errors.Is(err, rp.ErrRules) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The same checks as Parse still apply: signature, seq.
	if _, err := rp.ParseAt(rp.Sign(priv, fileBody(1, rulesNow.Add(time.Hour), "")), pub, 5, clk.now); !errors.Is(err, rp.ErrRules) {
		t.Errorf("an old seq: %v", err)
	}
	otherPub, _ := keys(t)
	if _, err := rp.ParseAt(rp.Sign(priv, fileBody(1, rulesNow.Add(time.Hour), "")), otherPub, 0, clk.now); !errors.Is(err, rp.ErrRules) {
		t.Errorf("a foreign key: %v", err)
	}
	// A file that is already expired when read is accepted: it only tightens.
	r, err = rp.ParseAt(rp.Sign(priv, fileBody(3, rulesNow.Add(-time.Hour), "")), pub, 0, clk.now)
	if err != nil || !r.Expired() {
		t.Errorf("an expired file: %v expired=%v", err, r != nil && r.Expired())
	}
}

// H3a: expiry never loosens. A verified entry stops counting; the restrictions of the file stay.
func TestExpiryNeverLoosens(t *testing.T) {
	pub, priv := keys(t)
	clk := &clockAt{rulesNow}
	r, err := rp.ParseAt(rp.Sign(priv, fileBody(1, rulesNow.Add(30*24*time.Hour), "")), pub, 0, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	entry := netip.MustParseAddr("198.51.100.77")
	if !r.HasVerifiedEntries() || !r.EntryAllowed("RU", rp.NetMobile, entry) {
		t.Fatal("a fresh file verifies the entry")
	}
	before := r.Decide("RU", rp.NetWired, rp.DestForeign)
	if before.UDP || before.TCPLimit != 0 && before.TCPLimit > 20000 {
		t.Fatalf("the file's udp_denied is in force: %+v", before)
	}

	clk.t = rulesNow.Add(30*24*time.Hour - time.Second)
	if r.Expired() || !r.EntryAllowed("RU", rp.NetMobile, entry) {
		t.Fatal("one second before valid_until the file is valid")
	}
	clk.t = rulesNow.Add(30 * 24 * time.Hour) // exactly valid_until: expired
	if !r.Expired() {
		t.Fatal("at valid_until the file is expired")
	}
	if r.EntryAllowed("RU", rp.NetMobile, entry) || r.HasVerifiedEntries() {
		t.Fatal("an expired file verifies no entry")
	}
	after := r.Decide("RU", rp.NetWired, rp.DestForeign)
	if after.UDP || after.TCPLimit != before.TCPLimit {
		t.Fatalf("expiry loosened the table: before %+v after %+v", before, after)
	}
	m := r.Decide("RU", rp.NetMobile, rp.DestForeign)
	if m.TCPLimit == 0 || m.TCPLimit > 20000 {
		t.Fatalf("the mobile cap of the file is gone after expiry: %+v", m)
	}
}

func TestParseWithoutValidUntilNeverExpires(t *testing.T) {
	pub, priv := keys(t)
	r, err := rp.Parse(rp.Sign(priv, []byte(`{"seq":1,"allowed_cidrs":["198.51.100.0/24"]}`)), pub, 0)
	if err != nil || r.Expired() || !r.ValidUntil().IsZero() || !r.HasVerifiedEntries() {
		t.Fatalf("Parse: %v expired=%v", err, r != nil && r.Expired())
	}
}
