package exit

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

func TestTCPConnectEncoding(t *testing.T) {
	t.Parallel()

	m := TCPConnect{Host: "192.0.2.53", Port: 53}
	want := mustHex(t, "0a0a3139322e302e322e35331035")
	if got := m.Append(nil); !bytes.Equal(got, want) {
		t.Fatalf("TCP_CONNECT = %x, want %x", got, want)
	}
	got, err := ParseTCPConnect(want)
	if err != nil || got != m {
		t.Fatalf("ParseTCPConnect = %+v, %v; want %+v", got, err, m)
	}

	// Flags are kept but unknown bits are ignored on receipt (objects.md §2 extension rules).
	in := mustHex(t, "0a0a3139322e302e322e353310351801")
	got, err = ParseTCPConnect(in)
	if err != nil {
		t.Fatalf("ParseTCPConnect(flags) = %v", err)
	}
	if got.Flags != 1 {
		t.Fatalf("flags = %d, want 1", got.Flags)
	}
}

func TestResultAndUDPEncodings(t *testing.T) {
	t.Parallel()

	r := Result{Code: ResultRateLimited}
	if got, want := r.Append(nil), mustHex(t, "0807"); !bytes.Equal(got, want) {
		t.Fatalf("RESULT = %x, want %x", got, want)
	}
	if got, err := ParseResult(r.Append(nil)); err != nil || got != r {
		t.Fatalf("ParseResult = %+v, %v", got, err)
	}
	// code 0 is the default and is omitted.
	if got := (Result{}).Append(nil); len(got) != 0 {
		t.Fatalf("RESULT{0} = %x, want empty", got)
	}
	if got, err := ParseResult(nil); err != nil || got.Code != 0 {
		t.Fatalf("ParseResult(nil) = %+v, %v", got, err)
	}

	u := UDPOpen{Req: 1}
	if got, want := u.Append(nil), mustHex(t, "0801"); !bytes.Equal(got, want) {
		t.Fatalf("UDP_OPEN = %x, want %x", got, want)
	}
	if got, err := ParseUDPOpen(u.Append(nil)); err != nil || got != u {
		t.Fatalf("ParseUDPOpen = %+v, %v", got, err)
	}

	ok := UDPOK{Req: 1, Session: 5}
	if got, want := ok.Append(nil), mustHex(t, "08011005"); !bytes.Equal(got, want) {
		t.Fatalf("UDP_OK = %x, want %x", got, want)
	}
	if got, err := ParseUDPOK(ok.Append(nil)); err != nil || got != ok {
		t.Fatalf("ParseUDPOK = %+v, %v", got, err)
	}

	pkt := UDPPkt{Addr: "192.0.2.53:53", Data: []byte{0xab, 0xcd}}
	want := mustHex(t, "0a0e3139322e302e322e35333a35331202abcd")
	if got := pkt.Append(nil); !bytes.Equal(got, want) {
		t.Fatalf("UDP_PKT = %x, want %x", got, want)
	}
	got, err := ParseUDPPkt(want)
	if err != nil || got.Addr != pkt.Addr || !bytes.Equal(got.Data, pkt.Data) {
		t.Fatalf("ParseUDPPkt = %+v, %v", got, err)
	}
}

func TestAccountEncoding(t *testing.T) {
	t.Parallel()

	a := Account{Price: 40, Credit: 1 << 20, FreeLeft: 1024, PayAddress: "EQD", PayTag: "ef2:0011223344556677",
		MinPay: 1000, Assets: []uint32{1, 2}, Vault: []byte{0xde, 0xad}}
	got, err := ParseAccount(a.Append(nil))
	if err != nil {
		t.Fatalf("ParseAccount: %v", err)
	}
	if got.Price != a.Price || got.Credit != a.Credit || got.FreeLeft != a.FreeLeft ||
		got.PayAddress != a.PayAddress || got.PayTag != a.PayTag || got.MinPay != a.MinPay ||
		len(got.Assets) != 2 || got.Assets[0] != 1 || got.Assets[1] != 2 || !bytes.Equal(got.Vault, a.Vault) {
		t.Fatalf("ACCOUNT round trip = %+v, want %+v", got, a)
	}
	// Assets are packed: one length-delimited field 7 carrying 01 02.
	if !bytes.Contains(a.Append(nil), mustHex(t, "3a020102")) {
		t.Fatalf("assets are not packed: %x", a.Append(nil))
	}
	// A zero account has an empty body.
	if got := (Account{}).Append(nil); len(got) != 0 {
		t.Fatalf("ACCOUNT{} = %x, want empty", got)
	}
}

func TestVoucherEncoding(t *testing.T) {
	t.Parallel()

	v := Voucher{Req: 7, Owner: []byte{1, 2}, PubKey: bytes.Repeat([]byte{3}, 32),
		Cumulative: []byte{9, 9}, Sig: bytes.Repeat([]byte{4}, 64), CodeHash: []byte{5}}
	got, err := ParseVoucher(v.Append(nil))
	if err != nil {
		t.Fatalf("ParseVoucher: %v", err)
	}
	if got.Req != v.Req || !bytes.Equal(got.Owner, v.Owner) || !bytes.Equal(got.PubKey, v.PubKey) ||
		!bytes.Equal(got.Cumulative, v.Cumulative) || !bytes.Equal(got.Sig, v.Sig) ||
		!bytes.Equal(got.CodeHash, v.CodeHash) {
		t.Fatalf("VOUCHER round trip = %+v, want %+v", got, v)
	}
	vr := VoucherResult{Req: 7, OK: true, Credit: 4096, Cumulative: []byte{9}}
	if got, err := ParseVoucherResult(vr.Append(nil)); err != nil || !got.OK || got.Req != 7 || got.Credit != 4096 {
		t.Fatalf("VOUCHER_RESULT = %+v, %v", got, err)
	}
}

func TestFramesRejectMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		call func([]byte) error
		in   []byte
	}{
		{"tcp short tag", func(b []byte) error { _, err := ParseTCPConnect(b); return err }, []byte{0x0a}},
		{"tcp bad utf8 host", func(b []byte) error { _, err := ParseTCPConnect(b); return err }, mustHex(t, "0a02c328")},
		{"tcp host too long", func(b []byte) error { _, err := ParseTCPConnect(b); return err },
			append(append([]byte{0x0a, 254}, bytes.Repeat([]byte{'a'}, 254)...), 0x10, 80)},
		{"tcp port zero", func(b []byte) error { _, err := ParseTCPConnect(b); return err }, mustHex(t, "0a01611000")},
		{"tcp port too big", func(b []byte) error { _, err := ParseTCPConnect(b); return err }, mustHex(t, "0a016110808004")},
		{"result wrong type", func(b []byte) error { _, err := ParseResult(b); return err }, mustHex(t, "1201ff")},
		{"udp_open short", func(b []byte) error { _, err := ParseUDPOpen(b); return err }, mustHex(t, "0801")[:1]},
		{"udp_ok bad req type", func(b []byte) error { _, err := ParseUDPOK(b); return err }, mustHex(t, "0a0101")},
		{"udp_pkt bad addr", func(b []byte) error { _, err := ParseUDPPkt(b); return err }, mustHex(t, "0a02c328")},
		{"udp_pkt huge data", func(b []byte) error { _, err := ParseUDPPkt(b); return err },
			append(mustHex(t, "0a0161"), mustHex(t, "12808004")...)},
		{"account asset type", func(b []byte) error { _, err := ParseAccount(b); return err }, mustHex(t, "0a0101")},
		{"voucher bad req", func(b []byte) error { _, err := ParseVoucher(b); return err }, mustHex(t, "0a0101")},
		{"voucher result bad ok", func(b []byte) error { _, err := ParseVoucherResult(b); return err }, mustHex(t, "08011002")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(tc.in)
			if err == nil {
				t.Fatalf("%x: want an error", tc.in)
			}
			if !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("%x: %v is not ErrMalformed", tc.in, err)
			}
		})
	}

	// Host of exactly 253 bytes is allowed.
	host := bytes.Repeat([]byte{'a'}, 253)
	ok := append([]byte{0x0a, 253}, host...)
	ok = append(ok, 0x10, 80)
	if _, err := ParseTCPConnect(ok); err != nil {
		t.Fatalf("253-byte host rejected: %v", err)
	}
}
