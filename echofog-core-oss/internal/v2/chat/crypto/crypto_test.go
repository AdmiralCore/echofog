package chatcrypto

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// The label constants are the ones the vector pins: SK = HKDF(0xFF*32 || DH1..DH4 || SS, zero salt,
// "echofog/chat2/pqxdh" || KEM key id || SHA-256(KEM ciphertext)).
func TestLabelsMatchVector(t *testing.T) {
	var v map[string]string
	vector(t, "chat2_pqxdh", &v)
	ikm := repeat(0xff, 32)
	for _, k := range []string{"dh1", "dh2", "dh3", "dh4", "kem_ss"} {
		ikm = append(ikm, hexBytes(t, v[k])...)
	}
	info := append([]byte(Labels.PQXDH), hexBytes(t, v["kem_key_id"])...)
	info = append(info, hexBytes(t, v["kem_ct_hash"])...)
	sk := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, make([]byte, 32), info), sk); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sk); got != v["SK"] {
		t.Fatalf("SK with the PQXDH label = %s, vector %s", got, v["SK"])
	}
	if Labels.Session != LabelPQXDH || Labels.InitChains != "echofog/chat2/ratchet-init" || Labels.Root != "echofog/chat2/ratchet-root" {
		t.Fatalf("labels: %+v", Labels)
	}
}

func TestPrekeysObject(t *testing.T) {
	clock := newClock()
	u := newUser(t, clock)
	raw, err := u.c.Prekeys()
	if err != nil {
		t.Fatal(err)
	}
	p, err := objects.VerifyPrekeys(raw, clock.Now())
	if err != nil {
		t.Fatalf("the published prekeys do not verify: %v", err)
	}
	if p.DeviceID != u.device() {
		t.Fatal("prekeys/2 must be signed by the device key")
	}
	if len(p.Body.GetOpks()) != 0 {
		t.Fatal("the DHT copy carries one-time prekeys")
	}
	if len(p.Body.GetKem()) != 1184 || len(p.Body.GetSpk()) != 32 {
		t.Fatalf("spk/kem sizes %d/%d", len(p.Body.GetSpk()), len(p.Body.GetKem()))
	}
	if got := int64(p.Body.GetSpkCreated()); got != clock.Now().Unix() {
		t.Fatalf("spk_created %d, clock %d", got, clock.Now().Unix())
	}
	if got := int64(p.Body.GetKemCreated()); got != clock.Now().Unix() {
		t.Fatalf("kem_created %d, clock %d", got, clock.Now().Unix())
	}
	if life := time.Unix(int64(p.Env.NotAfter), 0).Sub(clock.Now()); life > 30*24*time.Hour || life < 29*24*time.Hour {
		t.Fatalf("lifetime %v", life)
	}
	h, err := u.c.PrekeysHash()
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(raw); !bytes.Equal(h, want[:]) {
		t.Fatal("prekeys_hash is not SHA-256 of the envelope")
	}

	// the identity DH key is random per device
	other := newUser(t, clock)
	rawOther, _ := other.c.Prekeys()
	po, _ := objects.VerifyPrekeys(rawOther, clock.Now())
	if bytes.Equal(p.Body.GetIk(), po.Body.GetIk()) {
		t.Fatal("two devices share an identity DH key")
	}
	if _, err := keys.ParseKind(p.Body.GetIk(), keys.X25519); err != nil {
		t.Fatalf("ik: %v", err)
	}

	// the same object is returned while it stays valid, and the keys survive a restart
	again, _ := u.c.Prekeys()
	if !bytes.Equal(again, raw) {
		t.Fatal("a valid prekeys/2 was issued again")
	}
	u.restart()
	restarted, err := u.c.Prekeys()
	if err != nil {
		t.Fatal(err)
	}
	pr, err := objects.VerifyPrekeys(restarted, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pr.Body.GetIk(), p.Body.GetIk()) || !bytes.Equal(pr.Body.GetSpk(), p.Body.GetSpk()) || !bytes.Equal(pr.Body.GetKem(), p.Body.GetKem()) {
		t.Fatal("the keys changed over a restart")
	}
}

func TestPrekeysRenewed(t *testing.T) {
	clock := newClock()
	u := newUser(t, clock)
	first, _ := u.c.Prekeys()
	p1, _ := objects.VerifyPrekeys(first, clock.Now())

	clock.Advance(19 * 24 * time.Hour) // 11 days remain: kept
	same, _ := u.c.Prekeys()
	if !bytes.Equal(same, first) {
		t.Fatal("renewed too early")
	}
	clock.Advance(2 * 24 * time.Hour) // 9 days remain: renewed (objects.md 5, margin 10 days)
	next, err := u.c.Prekeys()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := objects.VerifyPrekeys(next, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p2.Env.Seq <= p1.Env.Seq || p2.Env.NotAfter <= p1.Env.NotAfter {
		t.Fatalf("a renewed object must have a higher seq and a later end: %d/%d", p1.Env.Seq, p2.Env.Seq)
	}
	if bytes.Equal(p1.Env.Raw, p2.Env.Raw) {
		t.Fatal("not renewed")
	}
}

func TestBundleHasOneFreshOneTimePrekey(t *testing.T) {
	clock := newClock()
	u := newUser(t, clock)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		raw, err := u.c.Bundle()
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 8256-16 {
			t.Fatalf("a bundle of %d bytes does not fit BUNDLE", len(raw))
		}
		p, err := objects.VerifyPrekeys(raw, clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Body.GetOpks()) != 1 {
			t.Fatalf("a bundle carries %d one-time prekeys, want 1", len(p.Body.GetOpks()))
		}
		o := string(p.Body.GetOpks()[0])
		if seen[o] {
			t.Fatal("a one-time prekey was handed out twice")
		}
		seen[o] = true
	}
}

func TestRoundTrip(t *testing.T) {
	a, b, init, _ := pair(t)

	if _, err := chatpb.ParseX3DHInit(init); err != nil {
		t.Fatalf("Start made an init that the parser refuses: %v", err)
	}
	got, err := accept(t, b, a, init)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("first message %q", got)
	}
	if node, ok := b.c.Peer(a.device()); !ok || node != a.node() {
		t.Fatal("the session is not bound to the sender's account")
	}
	if node, ok := a.c.Peer(b.device()); !ok || node != b.node() {
		t.Fatal("the session is not bound to the recipient's account")
	}

	for i := 0; i < 5; i++ {
		m, err := b.c.Encrypt(a.device(), []byte(fmt.Sprintf("b%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chatpb.ParseSecureMessage(m); err != nil {
			t.Fatalf("Encrypt made a message the parser refuses: %v", err)
		}
		p, err := a.c.Decrypt(b.device(), m)
		if err != nil || string(p) != fmt.Sprintf("b%d", i) {
			t.Fatalf("a decrypt: %q %v", p, err)
		}
		m, err = a.c.Encrypt(b.device(), []byte(fmt.Sprintf("a%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if p, err := b.c.Decrypt(a.device(), m); err != nil || string(p) != fmt.Sprintf("a%d", i) {
			t.Fatalf("b decrypt: %q %v", p, err)
		}
	}

	// out of order inside one chain
	m1, _ := a.c.Encrypt(b.device(), []byte("one"))
	m2, _ := a.c.Encrypt(b.device(), []byte("two"))
	if p, err := b.c.Decrypt(a.device(), m2); err != nil || string(p) != "two" {
		t.Fatalf("out of order: %q %v", p, err)
	}
	if p, err := b.c.Decrypt(a.device(), m1); err != nil || string(p) != "one" {
		t.Fatalf("late: %q %v", p, err)
	}
	// a replayed message is refused and costs nothing
	if _, err := b.c.Decrypt(a.device(), m2); !errors.Is(err, ErrRejected) {
		t.Fatalf("replay: %v", err)
	}

	// sessions survive a restart on both sides
	a.restart()
	b.restart()
	m, err := a.c.Encrypt(b.device(), []byte("after restart"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := b.c.Decrypt(a.device(), m); err != nil || string(p) != "after restart" {
		t.Fatalf("after restart: %q %v", p, err)
	}
}

func TestNoSession(t *testing.T) {
	clock := newClock()
	a, b := newUser(t, clock), newUser(t, clock)
	if _, err := a.c.Encrypt(b.device(), []byte("x")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := a.c.Decrypt(b.device(), []byte("x")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Decrypt: %v", err)
	}
	if _, ok := a.c.Peer(b.device()); ok {
		t.Fatal("a peer without a session")
	}
}

func TestStartRefusals(t *testing.T) {
	clock := newClock()
	a, b, c := newUser(t, clock), newUser(t, clock), newUser(t, clock)

	// a device the list does not vouch for
	if _, err := a.c.Start(b.bundle(), c.dl, []byte("x")); !errors.Is(err, ErrNotVouched) {
		t.Fatalf("not vouched: %v", err)
	}
	// prekeys that end by the node clock
	old := b.bundle()
	clock.Advance(31 * 24 * time.Hour)
	if _, err := a.c.Start(old, b.dl, []byte("x")); !errors.Is(err, ErrStale) {
		t.Fatalf("expired: %v", err)
	}
	// no session was created by any refusal
	if _, ok := a.c.Peer(b.device()); ok {
		t.Fatal("a refused start left a session")
	}
}

// A sender does not use a bundle whose keys the recipient has already dropped.
func TestStartPrekeyAge(t *testing.T) {
	clock := newClock()
	a, b := newUser(t, clock), newUser(t, clock)
	p := b.bundle()
	p.Body.SpkCreated = uint64(clock.Now().Add(-(MaxPrekeyAge + time.Hour)).Unix())
	if _, err := a.c.Start(p, b.dl, []byte("x")); !errors.Is(err, ErrStale) {
		t.Fatalf("an old signed prekey: %v", err)
	}
	p = b.bundle()
	p.Body.KemCreated = uint64(clock.Now().Add(-(MaxPrekeyAge + time.Hour)).Unix())
	if _, err := a.c.Start(p, b.dl, []byte("x")); !errors.Is(err, ErrStale) {
		t.Fatalf("an old KEM prekey: %v", err)
	}
	p = b.bundle()
	p.Body.SpkCreated = uint64(clock.Now().Add(time.Hour).Unix())
	if _, err := a.c.Start(p, b.dl, []byte("x")); !errors.Is(err, ErrStale) {
		t.Fatalf("a signed prekey from the future: %v", err)
	}
}

func TestStartRefusesOwnDevice(t *testing.T) {
	a := newUser(t, newClock())
	if _, err := a.c.Start(a.bundle(), a.dl, []byte("x")); err == nil {
		t.Fatal("a session with the own device")
	}
}

// Every failure of an incoming init is the same error with the same text, and none of them changes
// state: the one-time prekey is not used up, no session appears (AD20).
func TestAcceptRefusalsAreUniform(t *testing.T) {
	clock := newClock()
	a, b, c := newUser(t, clock), newUser(t, clock), newUser(t, clock)

	bundle := b.bundle()
	good, err := a.c.Start(bundle, b.dl, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := chatpb.ParseX3DHInit(good)

	mutate := func(f func(i *chatpb.X3DHInit)) []byte {
		i, _ := chatpb.ParseX3DHInit(good)
		f(i)
		out, err := objects.Marshal(i)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	flip := func(b []byte) []byte { b = append([]byte(nil), b...); b[len(b)/2] ^= 1; return b }
	rawA, _ := a.c.Prekeys()
	pa, _ := objects.VerifyPrekeys(rawA, clock.Now())
	rawC, _ := c.c.Prekeys()
	pc, _ := objects.VerifyPrekeys(rawC, clock.Now())

	cases := []struct {
		name   string
		sender *objects.Prekeys
		dl     *objects.DeviceList
		init   []byte
	}{
		{"garbage", pa, a.dl, []byte{1, 2, 3}},
		{"empty", pa, a.dl, nil},
		{"classic X3DH, no KEM part", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.KemCiphertext, i.KemKeyId = nil, nil })},
		{"identity key of another device", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.IdentityKey = repeat(7, 32) })},
		{"sender is not vouched for", pa, c.dl, good},
		{"sender is another device", pc, c.dl, good},
		{"unknown KEM key", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.KemKeyId = repeat(9, 32) })},
		{"bent KEM ciphertext", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.KemCiphertext = flip(i.KemCiphertext) })},
		{"unknown one-time prekey", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.OneTimeKeyId = repeat(9, 32) })},
		{"no one-time prekey where the bundle had one", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.OneTimeKeyId = nil })},
		{"bent first message", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.InitialCiphertext.Ciphertext = flip(i.InitialCiphertext.Ciphertext) })},
		{"another ephemeral key", pa, a.dl, mutate(func(i *chatpb.X3DHInit) { i.EphemeralKey = repeat(4, 32) })},
	}
	if parsed.GetOneTimeKeyId() == nil {
		t.Fatal("test setup: the bundle had no one-time prekey")
	}
	var want string
	for _, tc := range cases {
		_, err := b.c.Accept(tc.sender, tc.dl, tc.init)
		if !errors.Is(err, ErrRejected) {
			t.Errorf("%s: want ErrRejected, got %v", tc.name, err)
			continue
		}
		if want == "" {
			want = err.Error()
		}
		if err.Error() != want {
			t.Errorf("%s: %q differs from %q", tc.name, err.Error(), want)
		}
		if _, ok := b.c.Peer(a.device()); ok {
			t.Fatalf("%s: a refused init left a session", tc.name)
		}
	}

	// none of the failures used up the one-time prekey: the honest init still works
	got, err := b.c.Accept(pa, a.dl, good)
	if err != nil || string(got) != "hello" {
		t.Fatalf("the honest init after the failures: %q %v", got, err)
	}
}

// One-time prekeys are used exactly once, also when many inits name the same one at the same time.
func TestOneTimePrekeyUsedOnce(t *testing.T) {
	clock := newClock()
	b := newUser(t, clock)
	bundle := b.bundle()

	const n = 3
	var inits [][]byte
	var senders []*user
	for i := 0; i < n; i++ {
		a := newUser(t, clock)
		init, err := a.c.Start(bundle, b.dl, []byte("hi"))
		if err != nil {
			t.Fatal(err)
		}
		inits, senders = append(inits, init), append(senders, a)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := senders[i].c.Prekeys()
			p, _ := objects.VerifyPrekeys(raw, clock.Now())
			if _, err := b.c.Accept(p, senders[i].dl, inits[i]); err == nil {
				ok.Add(1)
			} else if !errors.Is(err, ErrRejected) {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d inits used the same one-time prekey, want exactly 1", ok.Load())
	}
}

func TestInitReplayIsADuplicate(t *testing.T) {
	a, b, init, _ := pair(t)
	if _, err := accept(t, b, a, init); err != nil {
		t.Fatal(err)
	}
	if _, err := accept(t, b, a, init); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("replayed init: %v", err)
	}
	// the session is intact
	m, _ := a.c.Encrypt(b.device(), []byte("next"))
	if p, err := b.c.Decrypt(a.device(), m); err != nil || string(p) != "next" {
		t.Fatalf("%q %v", p, err)
	}
	// also after a restart
	b.restart()
	if _, err := accept(t, b, a, init); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("replayed init after a restart: %v", err)
	}
}

// A decryption failure never deletes or damages the session (K8).
func TestDecryptFailureKeepsSession(t *testing.T) {
	a, b, init, _ := pair(t)
	if _, err := accept(t, b, a, init); err != nil {
		t.Fatal(err)
	}
	m, _ := a.c.Encrypt(b.device(), []byte("real"))
	bad := append([]byte(nil), m...)
	bad[len(bad)-3] ^= 0x55
	for i := 0; i < 5; i++ {
		if _, err := b.c.Decrypt(a.device(), bad); !errors.Is(err, ErrRejected) {
			t.Fatalf("bent message: %v", err)
		}
		if _, err := b.c.Decrypt(a.device(), []byte{1, 2, 3}); !errors.Is(err, ErrRejected) {
			t.Fatalf("garbage: %v", err)
		}
	}
	if p, err := b.c.Decrypt(a.device(), m); err != nil || string(p) != "real" {
		t.Fatalf("the session did not survive: %q %v", p, err)
	}
	if _, ok := b.c.Peer(a.device()); !ok {
		t.Fatal("the session is gone")
	}
}

// No (key, nonce) pair repeats: every message of a session has its own message key, and the nonces
// are random 24-byte values.
func TestCiphertextsDiffer(t *testing.T) {
	a, b, init, _ := pair(t)
	if _, err := accept(t, b, a, init); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	hdr := map[string]bool{}
	for i := 0; i < 200; i++ {
		m, err := a.c.Encrypt(b.device(), []byte("same plaintext"))
		if err != nil {
			t.Fatal(err)
		}
		sm, _ := chatpb.ParseSecureMessage(m)
		nonce := string(sm.Ciphertext[:24])
		if seen[nonce] {
			t.Fatal("a nonce repeated")
		}
		seen[nonce] = true
		h := fmt.Sprintf("%x/%d", sm.Header.RatchetKey, sm.Header.N)
		if hdr[h] {
			t.Fatal("a message counter repeated under one ratchet key")
		}
		hdr[h] = true
	}
}

// Store keys do not show whom the node talks to (security.md 4.8).
func TestStoreKeysAreBlinded(t *testing.T) {
	a, b, init, _ := pair(t)
	if _, err := accept(t, b, a, init); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*user{a, b} {
		other := b
		if u == b {
			other = a
		}
		id := other.device()
		node := other.node()
		for _, ns := range []string{KeysNamespace, SessionsNamespace} {
			entries, err := u.st.Store(ns).Scan(nil, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) == 0 && ns == SessionsNamespace {
				t.Fatalf("no session records in %s", ns)
			}
			for _, e := range entries {
				k := string(e.Key)
				if strings.Contains(k, string(id[:])) || strings.Contains(k, fmt.Sprintf("%x", id)) ||
					strings.Contains(k, string(node[:])) || strings.Contains(k, fmt.Sprintf("%x", node)) {
					t.Fatalf("%s: key %q shows a peer", ns, k)
				}
			}
		}
	}
}

// The signed prekey is replaced after a week, and the replaced one still opens inits made against
// the old bundle (it is kept 35 days).
func TestSignedPrekeyRotationKeepsOldKey(t *testing.T) {
	clock := newClock()
	a, b := newUser(t, clock), newUser(t, clock)
	oldBundle := b.bundle()
	init, err := a.c.Start(oldBundle, b.dl, []byte("from the old bundle"))
	if err != nil {
		t.Fatal(err)
	}
	b.c.ageKeysForTest(t, 8*24*time.Hour)
	clock.Advance(8 * 24 * time.Hour)
	raw, err := b.c.Prekeys()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := objects.VerifyPrekeys(raw, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fresh.Body.GetSpk(), oldBundle.Body.GetSpk()) || bytes.Equal(fresh.Body.GetKem(), oldBundle.Body.GetKem()) {
		t.Fatal("the signed and the KEM prekey were not replaced after a week")
	}
	if got := int64(fresh.Body.GetSpkCreated()); got != clock.Now().Unix() {
		t.Fatalf("spk_created %d, clock %d", got, clock.Now().Unix())
	}
	rawA, _ := a.c.Prekeys()
	pa, _ := objects.VerifyPrekeys(rawA, clock.Now())
	got, err := b.c.Accept(pa, a.dl, init)
	if err != nil || string(got) != "from the old bundle" {
		t.Fatalf("an init made against the replaced prekeys: %q %v", got, err)
	}
}

// A sender with the right construction but the v1 labels cannot talk to a chat/2 device, and one
// with the chat/2 labels can: the labels are the only difference.
func TestOnlyChat2LabelsInteroperate(t *testing.T) {
	clock := newClock()
	b := newUser(t, clock)
	bundle := b.bundle()

	for _, tc := range []struct {
		name   string
		v2     bool
		wantOK bool
	}{{"chat/2 labels", true, true}, {"v1 labels", false, false}} {
		s := newRawSender(t, clock, tc.v2)
		init := s.start(t, bundle, []byte("raw hello"))
		got, err := b.c.Accept(s.prekeys(t, clock), s.dl, init)
		switch {
		case tc.wantOK && (err != nil || string(got) != "raw hello"):
			t.Errorf("%s: %q %v", tc.name, got, err)
		case !tc.wantOK && !errors.Is(err, ErrRejected):
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// A classic X3DH init (no KEM part), as v1 builds it, is refused whatever the labels (services.md 4.4).
func TestClassicInitRefused(t *testing.T) {
	clock := newClock()
	b := newUser(t, clock)
	bundle := b.bundle()
	s := newRawSender(t, clock, true)
	init := s.startClassic(t, bundle, []byte("classic"))
	if _, err := chatpb.ParseX3DHInit(init); !errors.Is(err, chatpb.ErrInvalid) {
		t.Fatalf("the parser takes a classic init: %v", err)
	}
	if _, err := b.c.Accept(s.prekeys(t, clock), s.dl, init); !errors.Is(err, ErrRejected) {
		t.Fatalf("Accept: %v", err)
	}
}

// A fresh session replaces the old one, which is kept 7 days for messages still on the way
// (services.md 4.4).
func TestReplacedSessionKeptForLateMessages(t *testing.T) {
	a, b, init, clock := pair(t)
	if _, err := accept(t, b, a, init); err != nil {
		t.Fatal(err)
	}
	late, _ := b.c.Encrypt(a.device(), []byte("late, old session"))
	lateBack, _ := a.c.Encrypt(b.device(), []byte("late, old session, other way"))

	// b starts a fresh session and a accepts it
	raw, _ := a.c.Prekeys()
	pa, _ := objects.VerifyPrekeys(raw, clock.Now())
	init2, err := b.c.Start(pa, a.dl, []byte("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := accept(t, a, b, init2); err != nil || string(got) != "fresh" {
		t.Fatalf("fresh session: %q %v", got, err)
	}

	if p, err := a.c.Decrypt(b.device(), late); err != nil || string(p) != "late, old session" {
		t.Fatalf("a late message of the replaced session: %q %v", p, err)
	}
	if p, err := b.c.Decrypt(a.device(), lateBack); err != nil || string(p) != "late, old session, other way" {
		t.Fatalf("a late message to the starter: %q %v", p, err)
	}
	// the new session is the current one and keeps working
	m, _ := a.c.Encrypt(b.device(), []byte("on the new session"))
	if p, err := b.c.Decrypt(a.device(), m); err != nil || string(p) != "on the new session" {
		t.Fatalf("%q %v", p, err)
	}
	// and a late message does not move it
	m2, _ := b.c.Encrypt(a.device(), []byte("new, from b"))
	if p, err := a.c.Decrypt(b.device(), m2); err != nil || string(p) != "new, from b" {
		t.Fatalf("%q %v", p, err)
	}
}

func TestReplacedSessionExpiresAfterSevenDays(t *testing.T) {
	a, b, init, clock := pair(t)
	if _, err := accept(t, b, a, init); err != nil {
		t.Fatal(err)
	}
	late, _ := b.c.Encrypt(a.device(), []byte("late"))
	raw, _ := a.c.Prekeys()
	pa, _ := objects.VerifyPrekeys(raw, clock.Now())
	init2, _ := b.c.Start(pa, a.dl, []byte("fresh"))
	if _, err := accept(t, a, b, init2); err != nil {
		t.Fatal(err)
	}
	clock.Advance(7*24*time.Hour + time.Minute)
	if _, err := a.c.Decrypt(b.device(), late); !errors.Is(err, ErrRejected) {
		t.Fatalf("a message of a session replaced more than 7 days ago: %v", err)
	}
	// the old session is gone from the store
	entries, _ := a.st.Store(SessionsNamespace).Scan(nil, 1000)
	var prev int
	for _, e := range entries {
		if strings.HasPrefix(string(e.Key), "prev:") {
			prev++
		}
	}
	if prev != 0 {
		t.Fatalf("%d records of the expired session are left", prev)
	}
}
