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

type kdfSession struct {
	H, KI2R, KR2I, SessionSecret, SID, KPath, CBNew, PathMAC, PathBindFrame string
	PathID                                                                  uint64
	Role                                                                    byte
}

type kdfFile struct {
	Session kdfSession
	Sealed  struct{ Plaintext, Sealed, Record string }
}

// loadKDF reads kdf.json. Its keys contain commas, which encoding/json struct tags cannot
// express, so the sections are decoded as maps.
func loadKDF(t testing.TB) kdfFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "kdf.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Session map[string]any `json:"session"`
		Sealed  map[string]any `json:"sealed_record"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	str := func(m map[string]any, k string) string {
		v, ok := m[k].(string)
		if !ok {
			t.Fatalf("kdf.json: missing %q", k)
		}
		return v
	}
	ss := raw.Session
	v := kdfFile{Session: kdfSession{
		H: str(ss, "h"), KI2R: str(ss, "split_k_i2r (noise HKDF(ck, empty).o1)"),
		KR2I: str(ss, "split_k_r2i (noise HKDF(ck, empty).o2)"), SessionSecret: str(ss, "session_secret (noise HKDF(ck, label).o1)"),
		SID: str(ss, "sid"), KPath: str(ss, "k_path"), CBNew: str(ss, "cb_new"), PathMAC: str(ss, "path_mac"),
		PathBindFrame: str(ss, "PATH_BIND_frame"), PathID: uint64(ss["path_id"].(float64)), Role: byte(ss["role"].(float64)),
	}}
	v.Sealed.Plaintext = str(raw.Sealed, "plaintext")
	v.Sealed.Sealed = str(raw.Sealed, "sealed")
	v.Sealed.Record = str(raw.Sealed, "record (u16 len ‖ kind 1 ‖ sealed)")
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
	sealed, err := snd.Seal(nil, hx(t, v.Sealed.Plaintext))
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
	// The responder opens it.
	rcv, _ := NewSealer(rk)
	kind, body, err := readRecord(bytes.NewReader(out.Bytes()), nil)
	if err != nil || kind != RecordSealed {
		t.Fatal(kind, err)
	}
	pt, err := rcv.Open(nil, body)
	if err != nil || !bytes.Equal(pt, hx(t, v.Sealed.Plaintext)) {
		t.Fatalf("open %x %v", pt, err)
	}
	// The plaintext is connection-level mux frames: MUX_STREAM on stream 4.
	f, _, err := frame.ParseFrame(pt, nil)
	if err != nil || f.Type != frame.TypeMuxStream {
		t.Fatal(err)
	}
}

func TestSealerReplayWindow(t *testing.T) {
	ik, rk := randomKeys(t)
	snd, _ := NewSealer(ik)
	rcv, _ := NewSealer(rk)
	var recs [][]byte
	for i := 0; i < 5000; i++ {
		s, _ := snd.Seal(nil, []byte{byte(i)})
		recs = append(recs, s)
	}
	// Out of order within the window is fine; duplicates are not.
	if _, err := rcv.Open(nil, recs[10]); err != nil {
		t.Fatal(err)
	}
	if _, err := rcv.Open(nil, recs[3]); err != nil {
		t.Fatal(err)
	}
	if _, err := rcv.Open(nil, recs[10]); !errors.Is(err, ErrReplay) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := rcv.Open(nil, recs[4999]); err != nil {
		t.Fatal(err)
	}
	// 4999 − 4096 = 903: number 903 is just outside the window, 904 just inside.
	if _, err := rcv.Open(nil, recs[903]); !errors.Is(err, ErrReplay) {
		t.Fatalf("outside the window: %v", err)
	}
	if _, err := rcv.Open(nil, recs[904]); err != nil {
		t.Fatalf("inside the window: %v", err)
	}
	// A forged record is refused and does not advance the window.
	bad := append([]byte{}, recs[4998]...)
	bad[len(bad)-1] ^= 1
	if _, err := rcv.Open(nil, bad); err == nil {
		t.Fatal("forged record opened")
	}
	if _, err := rcv.Open(nil, recs[4998]); err != nil {
		t.Fatalf("genuine record after a forgery: %v", err)
	}
	// The other direction's key does not open it (keys are per direction).
	if _, err := snd.Open(nil, recs[1]); err == nil {
		t.Fatal("opened with the sender's own receive key")
	}
}

func TestSealerCounterLimit(t *testing.T) {
	ik, _ := randomKeys(t)
	snd, _ := NewSealer(ik)
	snd.next.Store(MaxSealCounter - 1)
	if _, err := snd.Seal(nil, []byte("last")); err != nil {
		t.Fatal(err)
	}
	if _, err := snd.Seal(nil, []byte("one too many")); !errors.Is(err, ErrCounterExhausted) {
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
		ra.StartSealing(sa)
		ra.Write([]byte("sealed bytes"))
		// A kind-0 record after the switch ends the path at the receiver.
		writeRecord(a, RecordPlain, []byte("late"))
	}()
	pt, err := rb.ReadPlain()
	if err != nil || len(pt) == 0 {
		t.Fatal(err)
	}
	sb, _ := NewSealer(rk)
	rb.StartSealing(sb)
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
	w.StartSealing(sa)
	w.Write([]byte("once"))
	rec := append([]byte{}, wire.Bytes()...)
	wire.Write(rec) // a relay repeats the record
	r := NewRecordConn(nopCloser{&wire})
	sb, _ := NewSealer(rk)
	r.StartSealing(sb)
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
