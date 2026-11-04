package mux

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// kdf.json uses plain keys (wire.md vectors, spec S20), so struct tags work.
type kdfSession struct {
	H             string `json:"h"`
	KI2R          string `json:"split_k_i2r"`
	KR2I          string `json:"split_k_r2i"`
	SessionSecret string `json:"session_secret"`
	SID           string `json:"sid"`
	KPath         string `json:"k_path"`
	CBNew         string `json:"cb_new"`
	PathID        uint64 `json:"path_id"`
	Role          byte   `json:"role"`
	PathMAC       string `json:"path_mac"`
	PathBindFrame string `json:"PATH_BIND_frame"`
}

type kdfFile struct {
	Session kdfSession `json:"session"`
	Sealed  struct {
		N         uint64 `json:"n"`
		Plaintext string `json:"plaintext"`
		Sealed    string `json:"sealed"`
		AAD       string `json:"aad"`
		Record    string `json:"record"`
	} `json:"sealed_record"`
}

func loadKDF(t testing.TB) kdfFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "kdf.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v kdfFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Session.SID == "" || v.Session.KI2R == "" || v.Session.SessionSecret == "" || v.Sealed.Record == "" || v.Sealed.AAD == "" {
		t.Fatal("kdf.json fields missing")
	}
	return v
}

func hx(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeKeys stands in for the handshake: the values of kdf.json "session".
type fakeKeys struct {
	sid       [16]byte
	initiator bool
	secret    []byte
	h         []byte
	i2r, r2i  []byte
}

func (k *fakeKeys) SID() [16]byte                   { return k.sid }
func (k *fakeKeys) Initiator() bool                 { return k.initiator }
func (k *fakeKeys) SessionSecret() []byte           { return k.secret }
func (k *fakeKeys) TranscriptHash() []byte          { return k.h }
func (k *fakeKeys) TransportKeys() ([]byte, []byte) { return k.i2r, k.r2i }

// vectorKeys returns the initiator's and the responder's view of the kdf.json session.
func vectorKeys(t testing.TB) (*fakeKeys, *fakeKeys) {
	v := loadKDF(t).Session
	k := &fakeKeys{initiator: true, secret: hx(t, v.SessionSecret), h: hx(t, v.H), i2r: hx(t, v.KI2R), r2i: hx(t, v.KR2I)}
	copy(k.sid[:], hx(t, v.SID))
	r := *k
	r.initiator = false
	return k, &r
}

// randomKeys returns a fresh pair of matching key views (another session).
func randomKeys(t testing.TB) (*fakeKeys, *fakeKeys) {
	k := &fakeKeys{initiator: true, secret: make([]byte, 32), h: make([]byte, 32), i2r: make([]byte, 32), r2i: make([]byte, 32)}
	for _, b := range [][]byte{k.sid[:], k.secret, k.h, k.i2r, k.r2i} {
		rand.Read(b)
	}
	r := *k
	r.initiator = false
	return k, &r
}

func TestDeriveSIDAndPathBindVector(t *testing.T) {
	v := loadKDF(t).Session
	ik, _ := vectorKeys(t)
	if sid := DeriveSID(ik.secret, ik.h); !bytes.Equal(sid[:], hx(t, v.SID)) {
		t.Fatalf("sid %x", sid)
	}
	kPath := PathBindKey(ik)
	if !bytes.Equal(kPath, hx(t, v.KPath)) {
		t.Fatalf("k_path %x", kPath)
	}
	var cb [32]byte
	copy(cb[:], hx(t, v.CBNew))
	mac := PathMAC(kPath, cb, ik.sid, v.PathID, v.Role)
	if !bytes.Equal(mac[:], hx(t, v.PathMAC)) {
		t.Fatalf("mac %x", mac)
	}
	f := frame.AppendFrame(nil, frame.Frame{Type: frame.TypePathBind,
		Payload: frame.PathBind{SID: ik.sid, PathID: v.PathID, MAC: mac}.Append(nil)})
	if !bytes.Equal(f, hx(t, v.PathBindFrame)) {
		t.Fatalf("PATH_BIND frame %x", f)
	}
}

func TestSealedRecordVector(t *testing.T) {
	v := loadKDF(t)
	ik, rk := vectorKeys(t)
	snd, err := NewSealer(ik)
	if err != nil {
		t.Fatal(err)
	}
	// aad = sid ‖ u64(n) ‖ u8(class 1) ‖ varint(path 0) ‖ varint(pipe 0)
	if aad := snd.AAD(v.Sealed.N, ClassRecord, 0, 0); !bytes.Equal(aad, hx(t, v.Sealed.AAD)) {
		t.Fatalf("aad %x", aad)
	}
	sealed, err := snd.Seal(nil, hx(t, v.Sealed.Plaintext), ClassRecord, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sealed, hx(t, v.Sealed.Sealed)) {
		t.Fatalf("sealed %x", sealed)
	}
	var out bytes.Buffer
	if err := writeRecord(&out, RecordSealed, sealed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), hx(t, v.Sealed.Record)) {
		t.Fatalf("record %x", out.Bytes())
	}
	// The RecordConn produces the same bytes.
	var rcOut bytes.Buffer
	snd2, _ := NewSealer(ik)
	w := NewRecordConn(nopCloser{&rcOut})
	w.StartSealing(snd2, 0, 0)
	w.Write(hx(t, v.Sealed.Plaintext))
	if !bytes.Equal(rcOut.Bytes(), hx(t, v.Sealed.Record)) {
		t.Fatalf("RecordConn record %x", rcOut.Bytes())
	}
	// The responder opens it, and only as a record of path 0, pipe 0.
	rcv, _ := NewSealer(rk)
	kind, body, err := readRecord(bytes.NewReader(out.Bytes()), nil)
	if err != nil || kind != RecordSealed {
		t.Fatal(kind, err)
	}
	var st PipeState
	if _, err := rcv.OpenRecord(nil, body, 1, 0, &st); err == nil {
		t.Fatal("record opened on another path")
	}
	if _, err := rcv.OpenRecord(nil, body, 0, 4, &st); err == nil {
		t.Fatal("record opened on another pipe")
	}
	if _, err := rcv.OpenDatagram(nil, body, 0); err == nil {
		t.Fatal("record opened as a datagram")
	}
	pt, err := rcv.OpenRecord(nil, body, 0, 0, &st)
	if err != nil || !bytes.Equal(pt, hx(t, v.Sealed.Plaintext)) {
		t.Fatalf("open %x %v", pt, err)
	}
	f, _, err := frame.ParseFrame(pt, nil)
	if err != nil || f.Type != frame.TypeMuxStream {
		t.Fatal(err)
	}
}

func TestOrderedPipeReplayRule(t *testing.T) {
	ik, rk := randomKeys(t)
	snd, _ := NewSealer(ik)
	rcv, _ := NewSealer(rk)
	// Two pipes share the counter: pipe 0 gets n = 0 and 10000, pipe 4 gets n = 1..9999.
	a0, _ := snd.Seal(nil, []byte("a0"), ClassRecord, 2, 0)
	var bs [][]byte
	for i := 0; i < 9999; i++ {
		b, _ := snd.Seal(nil, []byte("b"), ClassRecord, 2, 4)
		bs = append(bs, b)
	}
	a1, _ := snd.Seal(nil, []byte("a1"), ClassRecord, 2, 0)
	var pa, pb PipeState
	for _, b := range bs {
		if _, err := rcv.OpenRecord(nil, b, 2, 4, &pb); err != nil {
			t.Fatal(err)
		}
	}
	// Pipe 0 was read slowly: its records are far behind pipe 4, and still accepted (no window).
	if _, err := rcv.OpenRecord(nil, a0, 2, 0, &pa); err != nil {
		t.Fatalf("slow pipe lost a record: %v", err)
	}
	if _, err := rcv.OpenRecord(nil, a1, 2, 0, &pa); err != nil {
		t.Fatal(err)
	}
	// A repeat, or an older record, on the same pipe is refused.
	if _, err := rcv.OpenRecord(nil, a1, 2, 0, &pa); !errors.Is(err, ErrReplay) {
		t.Fatalf("repeat: %v", err)
	}
	if _, err := rcv.OpenRecord(nil, a0, 2, 0, &pa); !errors.Is(err, ErrReplay) {
		t.Fatalf("older: %v", err)
	}
	// A record moved to another pipe or path does not authenticate.
	var px PipeState
	if _, err := rcv.OpenRecord(nil, a1, 2, 4, &px); !errors.Is(err, ErrOpen) {
		t.Fatalf("moved pipe: %v", err)
	}
	if _, err := rcv.OpenRecord(nil, a1, 3, 0, &px); !errors.Is(err, ErrOpen) {
		t.Fatalf("moved path: %v", err)
	}
}

func TestDatagramReplayWindow(t *testing.T) {
	ik, rk := randomKeys(t)
	snd, _ := NewSealer(ik)
	rcv, _ := NewSealer(rk)
	var ds [][]byte
	for i := 0; i < 5000; i++ {
		d, _ := snd.Seal(nil, []byte{byte(i)}, ClassDatagram, 1, 0)
		ds = append(ds, d)
	}
	if _, err := rcv.OpenDatagram(nil, ds[10], 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rcv.OpenDatagram(nil, ds[3], 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rcv.OpenDatagram(nil, ds[10], 1); !errors.Is(err, ErrReplay) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := rcv.OpenDatagram(nil, ds[4999], 1); err != nil {
		t.Fatal(err)
	}
	// 4999 − 4096 = 903: just outside the window; 904 just inside.
	if _, err := rcv.OpenDatagram(nil, ds[903], 1); !errors.Is(err, ErrReplay) {
		t.Fatalf("outside the window: %v", err)
	}
	if _, err := rcv.OpenDatagram(nil, ds[904], 1); err != nil {
		t.Fatalf("inside the window: %v", err)
	}
	bad := append([]byte{}, ds[4998]...)
	bad[len(bad)-1] ^= 1
	if _, err := rcv.OpenDatagram(nil, bad, 1); err == nil {
		t.Fatal("forged datagram opened")
	}
	if _, err := rcv.OpenDatagram(nil, ds[4998], 1); err != nil {
		t.Fatalf("genuine datagram after a forgery: %v", err)
	}
	// A datagram is bound to its path.
	other, _ := snd.Seal(nil, []byte("x"), ClassDatagram, 1, 0)
	if _, err := rcv.OpenDatagram(nil, other, 3); !errors.Is(err, ErrOpen) {
		t.Fatalf("datagram moved to another path: %v", err)
	}
	if _, err := snd.OpenDatagram(nil, ds[1], 1); err == nil {
		t.Fatal("opened with the sender's own receive key")
	}
}

func TestSealerCounterLimit(t *testing.T) {
	ik, _ := randomKeys(t)
	snd, _ := NewSealer(ik)
	snd.next.Store(MaxSealCounter - 1)
	if _, err := snd.Seal(nil, []byte("last"), ClassRecord, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := snd.Seal(nil, []byte("one too many"), ClassDatagram, 0, 0); !errors.Is(err, ErrCounterExhausted) {
		t.Fatalf("%v", err)
	}
}

func TestRecordLimitsAndKinds(t *testing.T) {
	// Record length cap: u16(1 + len(body)) ≤ 16384 + 64.
	var b bytes.Buffer
	if err := writeRecord(&b, RecordSealed, make([]byte, MaxRecord)); err == nil {
		t.Fatal("oversized record written")
	}
	hdr := []byte{byte((MaxRecord + 1) >> 8), byte((MaxRecord + 1) & 0xff), RecordSealed}
	if _, _, err := readRecord(bytes.NewReader(append(hdr, make([]byte, MaxRecord)...)), nil); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversized record read: %v", err)
	}
	// Unknown kind ends the path.
	if _, _, err := readRecord(bytes.NewReader([]byte{0, 2, 7, 0}), nil); !errors.Is(err, ErrRecordKind) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, _, err := readRecord(bytes.NewReader([]byte{0, 0}), nil); !errors.Is(err, ErrRecordKind) {
		t.Fatalf("empty record: %v", err)
	}
	if _, _, err := readRecord(bytes.NewReader([]byte{0, 5, 1, 0}), nil); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated: %v", err)
	}
}

func TestRecordConnHandshakeThenSealed(t *testing.T) {
	ik, rk := randomKeys(t)
	a, b := pipeRW()
	ra := NewRecordConn(a)
	rb := NewRecordConn(b)
	go func() {
		ra.WritePlain(frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: []byte{1, 0, 1, 0, 1, 0}}))
		sa, _ := NewSealer(ik)
		ra.StartSealing(sa, 0, 0)
		ra.Write([]byte("sealed bytes"))
		// A kind-0 record after the switch ends the path at the receiver.
		writeRecord(a, RecordPlain, []byte("late"))
	}()
	pt, err := rb.ReadPlain()
	if err != nil || len(pt) == 0 {
		t.Fatal(err)
	}
	sb, _ := NewSealer(rk)
	rb.StartSealing(sb, 0, 0)
	got := make([]byte, 12)
	if _, err := io.ReadFull(rb, got); err != nil || string(got) != "sealed bytes" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := rb.Read(got); !errors.Is(err, ErrRecordKind) {
		t.Fatalf("kind 0 after sealing: %v", err)
	}
}

func TestRecordConnRefusesReplayedRecord(t *testing.T) {
	ik, rk := randomKeys(t)
	sa, _ := NewSealer(ik)
	var wire bytes.Buffer
	w := NewRecordConn(nopCloser{&wire})
	w.StartSealing(sa, 0, 0)
	w.Write([]byte("once"))
	rec := append([]byte{}, wire.Bytes()...)
	wire.Write(rec) // a relay repeats the record
	r := NewRecordConn(nopCloser{&wire})
	sb, _ := NewSealer(rk)
	r.StartSealing(sb, 0, 0)
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "once" {
		t.Fatal(err)
	}
	if _, err := r.Read(buf); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed record: %v", err)
	}
}

type nopCloser struct{ io.ReadWriter }

func (nopCloser) Close() error { return nil }

func pipeRW() (net.Conn, net.Conn) { return net.Pipe() }
