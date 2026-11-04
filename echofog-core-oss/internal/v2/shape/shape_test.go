package shape

import (
	"errors"
	"reflect"
	"testing"

	objectspb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

func rng(min, max uint32) *objectspb.Range { return &objectspb.Range{Min: min, Max: max} }

// TestBuiltinProfilesMatchWireTable pins the built-in table of wire.md §9 and registries.md §6.3.
func TestBuiltinProfilesMatchWireTable(t *testing.T) {
	udp := &Profile{
		ID:        IDDefaultUDP,
		InitLen:   &Range{1280, 1420},
		Pad:       &Range{0, 48},
		Junk:      &Junk{Count: Range{0, 2}, Len: Range{32, 256}, Gap: Range{5, 30}},
		Keepalive: Range{12, 24},
		IdlePad:   &Range{500, 4000},
	}
	tls := &Profile{
		ID:        IDDefaultTLS,
		Pad:       &Range{0, 256},
		Buckets:   []uint32{512, 1024, 1460, 2920, 4096},
		PadBudget: &Range{4096, 16384},
		Keepalive: Range{12, 24},
		IdlePad:   &Range{500, 4000},
	}
	none := &Profile{ID: IDNone, Keepalive: Range{12, 24}}

	cases := []struct {
		name string
		id   uint32
		want *Profile
	}{
		{"none", IDNone, none},
		{"default-udp", IDDefaultUDP, udp},
		{"default-tls", IDDefaultTLS, tls},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Builtin(tc.id)
			if err != nil {
				t.Fatalf("Builtin(%d): %v", tc.id, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Builtin(%d) = %+v, want %+v", tc.id, got, tc.want)
			}
		})
	}

	if _, err := Builtin(IDConstantRateCall); !errors.Is(err, ErrReservedProfile) {
		t.Fatalf("Builtin(%d) = %v, want ErrReservedProfile", IDConstantRateCall, err)
	}
	for _, id := range []uint32{4, 99, 0x8000, 0xffff} {
		if _, err := Builtin(id); !errors.Is(err, ErrUnknownProfile) {
			t.Errorf("Builtin(%d) = %v, want ErrUnknownProfile", id, err)
		}
	}
}

// TestBuiltinIsACopy guards callers from mutating the package's table through a returned profile.
func TestBuiltinIsACopy(t *testing.T) {
	p, err := Builtin(IDDefaultTLS)
	if err != nil {
		t.Fatal(err)
	}
	p.Buckets[0] = 1
	p.Pad.Min = 9
	again, err := Builtin(IDDefaultTLS)
	if err != nil {
		t.Fatal(err)
	}
	if again.Buckets[0] != 512 || again.Pad.Min != 0 {
		t.Fatalf("Builtin table was mutated: %+v", again)
	}
}

func TestParseBuiltinProfile(t *testing.T) {
	got, err := Parse(&objectspb.ShapeProfile{Id: IDDefaultUDP})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want, _ := Builtin(IDDefaultUDP)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(id=%d) = %+v, want %+v", IDDefaultUDP, got, want)
	}

	// nil and a zero profile are both "none" (registries.md §6.3).
	for _, p := range []*objectspb.ShapeProfile{nil, &objectspb.ShapeProfile{}} {
		got, err := Parse(p)
		if err != nil {
			t.Fatalf("Parse(%v): %v", p, err)
		}
		if got.ID != IDNone || got.InitLen != nil || got.Pad != nil || got.PadBudget != nil ||
			got.Junk != nil || len(got.Buckets) != 0 || len(got.Decoys) != 0 ||
			got.Keepalive != (Range{12, 24}) || got.IdlePad != nil {
			t.Fatalf("Parse(%v) = %+v, want none", p, got)
		}
	}
}

func TestParseCustomProfile(t *testing.T) {
	in := &objectspb.ShapeProfile{
		InitLen:   rng(1300, 1400),
		Pad:       rng(10, 20),
		Buckets:   []uint32{600, 1200},
		PadBudget: rng(0, 0),
		Junk: &objectspb.Junk{
			Count: rng(1, 3),
			Len:   rng(64, 200),
			GapMs: rng(1, 9),
		},
		Decoys: []*objectspb.Decoy{
			{Kind: DecoyRaw, Template: []byte{0, 0xff, 0xff, 1}},
			{Kind: DecoyDNS, Qname: "example.org."},
			{Kind: DecoyQUICInitial},
		},
		KeepaliveS: rng(30, 40),
		IdlePadMs:  rng(1000, 2000),
	}
	got, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Profile{
		ID:        IDNone,
		InitLen:   &Range{1300, 1400},
		Pad:       &Range{10, 20},
		Buckets:   []uint32{600, 1200},
		PadBudget: &Range{0, 0},
		Junk:      &Junk{Count: Range{1, 3}, Len: Range{64, 200}, Gap: Range{1, 9}},
		Decoys: []Decoy{
			{Kind: DecoyRaw, Template: []byte{0, 0xff, 0xff, 1}},
			{Kind: DecoyDNS, QName: "example.org."},
			{Kind: DecoyQUICInitial},
		},
		Keepalive: Range{30, 40},
		IdlePad:   &Range{1000, 2000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %+v, want %+v", got, want)
	}
}

func TestParseRejectsBuiltinWithData(t *testing.T) {
	cases := []struct {
		name string
		in   *objectspb.ShapeProfile
	}{
		{"init_len", &objectspb.ShapeProfile{Id: IDDefaultUDP, InitLen: rng(1280, 1420)}},
		{"pad", &objectspb.ShapeProfile{Id: IDDefaultUDP, Pad: rng(0, 1)}},
		{"buckets", &objectspb.ShapeProfile{Id: IDDefaultUDP, Buckets: []uint32{512}}},
		{"pad_budget", &objectspb.ShapeProfile{Id: IDDefaultUDP, PadBudget: rng(1, 2)}},
		{"junk", &objectspb.ShapeProfile{Id: IDDefaultTLS, Junk: &objectspb.Junk{}}},
		{"decoys", &objectspb.ShapeProfile{Id: IDDefaultUDP, Decoys: []*objectspb.Decoy{{Kind: DecoyQUICInitial}}}},
		{"keepalive", &objectspb.ShapeProfile{Id: IDDefaultUDP, KeepaliveS: rng(12, 24)}},
		{"idle_pad", &objectspb.ShapeProfile{Id: IDDefaultUDP, IdlePadMs: rng(500, 4000)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.in); !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("Parse(%+v) = %v, want ErrInvalidProfile", tc.in, err)
			}
		})
	}
}

// TestParseValidation checks the limits of wire.md §2.4, §10 and the resource bounds documented in
// doc.go, with one case per profile field.
func TestParseValidation(t *testing.T) {
	tooManyBuckets := make([]uint32, 33)
	for i := range tooManyBuckets {
		tooManyBuckets[i] = uint32(i + 1)
	}
	tooManyDecoys := make([]*objectspb.Decoy, 17)
	for i := range tooManyDecoys {
		tooManyDecoys[i] = &objectspb.Decoy{Kind: DecoyQUICInitial}
	}
	junk := func(count, length, gap *objectspb.Range) *objectspb.Junk {
		return &objectspb.Junk{Count: count, Len: length, GapMs: gap}
	}

	cases := []struct {
		name string
		in   *objectspb.ShapeProfile
	}{
		{"init_len below the 1271 floor", &objectspb.ShapeProfile{InitLen: rng(1270, 1300)}},
		{"init_len above the 1452 path MTU", &objectspb.ShapeProfile{InitLen: rng(1300, 1453)}},
		{"init_len min above max", &objectspb.ShapeProfile{InitLen: rng(1400, 1300)}},
		{"pad above the record limit", &objectspb.ShapeProfile{Pad: rng(0, 16385)}},
		{"pad min above max", &objectspb.ShapeProfile{Pad: rng(10, 9)}},
		{"buckets not ascending", &objectspb.ShapeProfile{Buckets: []uint32{1024, 512}}},
		{"buckets duplicate", &objectspb.ShapeProfile{Buckets: []uint32{512, 512}}},
		{"bucket zero", &objectspb.ShapeProfile{Buckets: []uint32{0, 512}}},
		{"bucket above the record limit", &objectspb.ShapeProfile{Buckets: []uint32{16385}}},
		{"too many buckets", &objectspb.ShapeProfile{Buckets: tooManyBuckets}},
		{"pad_budget min above max", &objectspb.ShapeProfile{PadBudget: rng(4096, 2048)}},
		{"junk without bounds", &objectspb.ShapeProfile{Junk: &objectspb.Junk{}}},
		{"junk count too large", &objectspb.ShapeProfile{Junk: junk(rng(0, 17), rng(32, 64), rng(1, 2))}},
		{"junk length empty", &objectspb.ShapeProfile{Junk: junk(rng(0, 2), rng(0, 64), rng(1, 2))}},
		{"junk length above a datagram", &objectspb.ShapeProfile{Junk: junk(rng(0, 2), rng(32, 1453), rng(1, 2))}},
		{"junk gap too large", &objectspb.ShapeProfile{Junk: junk(rng(0, 2), rng(32, 64), rng(1, 1001))}},
		{"junk gap min above max", &objectspb.ShapeProfile{Junk: junk(rng(0, 2), rng(32, 64), rng(10, 9))}},
		{"decoy unknown kind", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: 4}}}},
		{"decoy raw without template", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: DecoyRaw}}}},
		{"decoy raw with a query name", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: DecoyRaw, Template: []byte{1}, Qname: "example.org."}}}},
		{"decoy raw template too long", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: DecoyRaw, Template: make([]byte, 1453)}}}},
		{"decoy dns without qname", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: DecoyDNS}}}},
		{"decoy dns with a bad qname", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: DecoyDNS, Qname: "bad..name"}}}},
		{"decoy quic with a template", &objectspb.ShapeProfile{Decoys: []*objectspb.Decoy{{Kind: DecoyQUICInitial, Template: []byte{1}}}}},
		{"too many decoys", &objectspb.ShapeProfile{Decoys: tooManyDecoys}},
		{"keepalive zero", &objectspb.ShapeProfile{KeepaliveS: rng(0, 24)}},
		{"keepalive at the path timeout", &objectspb.ShapeProfile{KeepaliveS: rng(12, 60)}},
		{"keepalive min above max", &objectspb.ShapeProfile{KeepaliveS: rng(24, 12)}},
		{"idle_pad zero", &objectspb.ShapeProfile{IdlePadMs: rng(0, 4000)}},
		{"idle_pad min above max", &objectspb.ShapeProfile{IdlePadMs: rng(4000, 500)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.in); !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("Parse(%+v) = %v, want ErrInvalidProfile", tc.in, err)
			}
		})
	}

	// The same limits apply to a profile built by hand and validated directly.
	if err := (&Profile{Keepalive: Range{12, 60}}).Validate(); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("Validate = %v, want ErrInvalidProfile", err)
	}
	valid := &Profile{
		InitLen:   &Range{1271, 1452},
		Pad:       &Range{0, 16384},
		Buckets:   []uint32{1, 16384},
		PadBudget: &Range{0, 1 << 20},
		Junk:      &Junk{Count: Range{0, 16}, Len: Range{1, 1452}, Gap: Range{0, 1000}},
		Decoys:    []Decoy{{Kind: DecoyQUICInitial}},
		Keepalive: Range{1, 59},
		IdlePad:   &Range{1, 1 << 20},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) = %v", err)
	}
}

// TestParseKeepalivePresence checks that a present keepalive_s is valid only in [1, 59], while an
// absent field falls back to the wire.md §7.1 interval [12, 24] (decisions.md, T2.5, п. 3).
func TestParseKeepalivePresence(t *testing.T) {
	cases := []struct {
		name    string
		in      *objectspb.Range
		want    Range
		wantErr bool
	}{
		{"absent", nil, Range{12, 24}, false},
		{"present minimum", rng(1, 1), Range{1, 1}, false},
		{"present maximum", rng(59, 59), Range{59, 59}, false},
		{"present in range", rng(20, 30), Range{20, 30}, false},
		{"present zero", rng(0, 0), Range{}, true},
		{"present zero to default max", rng(0, 24), Range{}, true},
		{"present min above max", rng(24, 12), Range{}, true},
		{"present at the path timeout", rng(12, 60), Range{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(&objectspb.ShapeProfile{KeepaliveS: tc.in})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidProfile) {
					t.Fatalf("Parse(keepalive_s=%v) = %v, want ErrInvalidProfile", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(keepalive_s=%v): %v", tc.in, err)
			}
			if got.Keepalive != tc.want {
				t.Fatalf("Parse(keepalive_s=%v).Keepalive = %v, want %v", tc.in, got.Keepalive, tc.want)
			}
		})
	}
}
