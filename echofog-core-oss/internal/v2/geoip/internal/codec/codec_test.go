package codec

import (
	"bytes"
	"fmt"
	"net/netip"
	"testing"
)

func rec(start, end, country string) Record {
	return Record{
		Start:   netip.MustParseAddr(start),
		End:     netip.MustParseAddr(end),
		Country: country,
	}
}

func encodeDB(t testing.TB, records []Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Encode(&buf, records); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf.Bytes()
}

func decodeDB(t testing.TB, records []Record) *DB {
	t.Helper()
	db, err := Decode(encodeDB(t, records))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return db
}

func TestLookup(t *testing.T) {
	db := decodeDB(t, []Record{
		rec("1.0.0.0", "1.0.0.255", "AU"),
		rec("1.0.2.0", "1.0.3.255", "CN"),
		rec("2.0.0.0", "2.0.0.255", "US"),
		rec("2.0.1.0", "2.0.1.255", "US"), // adjacent: merged on encode
		rec("2001:4860::", "2001:4860:ffff:ffff:ffff:ffff:ffff:ffff", "US"),
		rec("2a00:1450::", "2a00:1450:ffff:ffff:ffff:ffff:ffff:ffff", "DE"),
	})

	cases := []struct {
		addr string
		want string
		ok   bool
	}{
		{"1.0.0.0", "AU", true},
		{"1.0.0.128", "AU", true},
		{"1.0.0.255", "AU", true},
		{"1.0.1.0", "", false}, // gap between ranges
		{"1.0.1.255", "", false},
		{"1.0.2.0", "CN", true},
		{"1.0.3.255", "CN", true},
		{"1.0.4.0", "", false},
		{"2.0.0.255", "US", true},
		{"2.0.1.0", "US", true}, // merged neighbour
		{"2.0.1.255", "US", true},
		{"2001:4860::", "US", true},
		{"2001:4860:4860::8888", "US", true},
		{"2001:4861::", "", false},
		{"2a00:1450::1", "DE", true},
		{"::ffff:1.0.0.1", "AU", true}, // IPv4-mapped
	}
	for _, tc := range cases {
		got, ok := db.Lookup(netip.MustParseAddr(tc.addr))
		if ok != tc.ok || got != tc.want {
			t.Errorf("Lookup(%s) = %q, %v; want %q, %v", tc.addr, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLookupAcrossBlocks exercises the block index: more records than one block holds, in both
// families, with every lookup landing on a block start, inside a block and between blocks.
func TestLookupAcrossBlocks(t *testing.T) {
	var records []Record
	for i := 0; i < 200; i++ {
		country := "US"
		if i%2 == 1 {
			country = "DE"
		}
		start := netip.AddrFrom4([4]byte{100, byte(i / 256), byte(i % 256), 0})
		end := netip.AddrFrom4([4]byte{100, byte(i / 256), byte(i % 256), 255})
		records = append(records, Record{Start: start, End: end, Country: country})
	}
	for i := 0; i < 200; i++ {
		country := "CN"
		if i%3 == 0 {
			country = "IR"
		}
		start := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, byte(i >> 8), byte(i)})
		end := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, byte(i >> 8), byte(i), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		records = append(records, Record{Start: start, End: end, Country: country})
	}
	db := decodeDB(t, records)

	for i := 0; i < 200; i++ {
		want := "US"
		if i%2 == 1 {
			want = "DE"
		}
		a := netip.AddrFrom4([4]byte{100, byte(i / 256), byte(i % 256), 1})
		if got, ok := db.Lookup(a); !ok || got != want {
			t.Fatalf("v4 Lookup(%s) = %q, %v; want %q", a, got, ok, want)
		}
		want = "CN"
		if i%3 == 0 {
			want = "IR"
		}
		a = netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, byte(i >> 8), byte(i), 0, 0, 0, 0, 0, 0, 0, 1})
		if got, ok := db.Lookup(a); !ok || got != want {
			t.Fatalf("v6 Lookup(%s) = %q, %v; want %q", a, got, ok, want)
		}
	}

	// The gap after the last IPv4 run (ranges stop at 100.0.199.255) and after the IPv6 run
	// are unknown.
	if _, ok := db.Lookup(netip.MustParseAddr("100.0.200.1")); ok {
		t.Fatal("address after the last range resolved")
	}
	if _, ok := db.Lookup(netip.MustParseAddr("2001:db8:0:201::1")); ok {
		t.Fatal("IPv6 address after the last range resolved")
	}
}

func TestLookupIPv6TopOfSpace(t *testing.T) {
	db := decodeDB(t, []Record{
		rec("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ff00", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "US"),
	})
	if got, ok := db.Lookup(netip.MustParseAddr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")); !ok || got != "US" {
		t.Fatalf("top-of-space Lookup = %q, %v; want US", got, ok)
	}
	if _, ok := db.Lookup(netip.MustParseAddr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:feff")); ok {
		t.Fatal("address below the range resolved")
	}
}

func TestEncodeRejectsBadRecords(t *testing.T) {
	cases := []struct {
		name    string
		records []Record
	}{
		{"mixed families", []Record{rec("1.0.0.0", "::1", "US")}},
		{"end before start", []Record{rec("1.0.1.0", "1.0.0.0", "US")}},
		{"unknown code", []Record{rec("1.0.0.0", "1.0.0.255", "ZZ")}},
		{"lowercase code", []Record{rec("1.0.0.0", "1.0.0.255", "us")}},
		{"one letter", []Record{rec("1.0.0.0", "1.0.0.255", "U")}},
		{"overlap", []Record{
			rec("1.0.0.0", "1.0.0.255", "US"),
			rec("1.0.0.128", "1.0.1.255", "DE"),
		}},
		{"same range twice", []Record{
			rec("1.0.0.0", "1.0.0.255", "US"),
			rec("1.0.0.0", "1.0.0.255", "US"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Encode(&bytes.Buffer{}, tc.records); err == nil {
				t.Fatal("Encode succeeded, want error")
			}
		})
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	valid := encodeDB(t, []Record{
		rec("1.0.0.0", "1.0.0.255", "US"),
		rec("2001:4860::", "2001:4860::ffff", "US"),
	})
	for i := 0; i < len(valid); i++ {
		if _, err := Decode(valid[:i]); err == nil {
			t.Fatalf("Decode of %d-byte prefix succeeded", i)
		}
	}
	cases := map[string]func([]byte) []byte{
		"trailing byte": func(b []byte) []byte { return append(b, 0) },
		"bad magic":     func(b []byte) []byte { b[0] = 'X'; return b },
		"bad version":   func(b []byte) []byte { b[4] = 2; return b },
		"reserved":      func(b []byte) []byte { b[5] = 1; return b },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			if _, err := Decode(mutate(data)); err == nil {
				t.Fatal("Decode succeeded, want error")
			}
		})
	}
}

func TestEmptyDatabase(t *testing.T) {
	db := decodeDB(t, nil)
	for _, s := range []string{"8.8.8.8", "2001:4860:4860::8888"} {
		if got, ok := db.Lookup(netip.MustParseAddr(s)); ok {
			t.Fatalf("Lookup(%s) = %q, true; want unknown", s, got)
		}
	}
}

func FuzzDecode(f *testing.F) {
	seed := encodeDB(f, []Record{
		rec("1.0.0.0", "1.0.0.255", "AU"),
		rec("2001:4860::", "2001:4860::ffff", "US"),
	})
	f.Add(seed)
	f.Add([]byte("EFGI"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		db, err := Decode(data)
		if err != nil {
			return
		}
		for _, s := range []string{"1.0.0.1", "8.8.8.8", "2001:4860::1", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"} {
			db.Lookup(netip.MustParseAddr(s))
		}
	})
}

// TestCountryIndexWidth guards the one-byte country index: the format cannot grow past 255
// countries without a version bump.
func TestCountryIndexWidth(t *testing.T) {
	var records []Record
	for i := 0; i < 256; i++ {
		code := fmt.Sprintf("%c%c", 'A'+byte(i/26), 'A'+byte(i%26))
		start := netip.AddrFrom4([4]byte{100, byte(i), 0, 0})
		end := netip.AddrFrom4([4]byte{100, byte(i), 0, 255})
		records = append(records, Record{Start: start, End: end, Country: code})
	}
	if err := Encode(&bytes.Buffer{}, records); err == nil {
		t.Fatal("Encode accepted 256 countries")
	}
}
