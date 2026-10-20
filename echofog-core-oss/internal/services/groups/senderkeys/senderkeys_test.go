package senderkeys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

type author struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newAuthor(t testing.TB, id string) author {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return author{id: id, pub: pub, priv: priv}
}

func (a author) sign(b []byte) ([]byte, error) { return ed25519.Sign(a.priv, b), nil }

func epochOf(t testing.TB, a author, n uint64, members ...string) *Epoch {
	t.Helper()
	e, err := NewEpoch("g1", n, a.id, members, time.Now(), a.sign)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// two members with the same epoch: what one seals the other opens, in order and out of it
func TestSealOpenInAndOutOfOrder(t *testing.T) {
	admin := newAuthor(t, "aa")
	e := epochOf(t, admin, 1, "bb", "aa")
	if err := e.Verify(admin.pub); err != nil {
		t.Fatal(err)
	}
	alice, bob := NewKeyring("g1", "aa"), NewKeyring("g1", "bb")
	if !alice.Add(e) || alice.Add(e) {
		t.Fatal("an epoch is new once")
	}
	if alice.LearnedAt(e) == 0 || NewKeyring("g1", "x").LearnedAt(e) != 0 {
		t.Fatal("when an epoch was learned is kept")
	}
	bob.Add(e)

	var envs []*Envelope
	for i := 0; i < 5; i++ {
		v, err := alice.Seal([]byte(fmt.Sprint("m", i)))
		if err != nil {
			t.Fatal(err)
		}
		if v.N != uint32(i) {
			t.Fatalf("message %d got index %d", i, v.N)
		}
		envs = append(envs, v)
	}
	// 0, then 3 (1 and 2 kept for later), then 2, 1, 4
	for _, i := range []int{0, 3, 2, 1, 4} {
		p, err := bob.Open(envs[i])
		if err != nil || string(p) != fmt.Sprint("m", i) {
			t.Fatalf("message %d: %q %v", i, p, err)
		}
	}
	// every key was used once
	for _, i := range []int{0, 1, 3} {
		if _, err := bob.Open(envs[i]); !errors.Is(err, ErrOldMessage) {
			t.Fatalf("message %d opened twice: %v", i, err)
		}
	}
	// the sender can read its own, as a member handing it on would
	if p, err := NewKeyringWith(t, e, "cc").Open(envs[4]); err != nil || string(p) != "m4" {
		t.Fatalf("a third member: %q %v", p, err)
	}
}

func NewKeyringWith(t testing.TB, e *Epoch, self string) *Keyring {
	k := NewKeyring(e.Group, self)
	k.Add(e)
	return k
}

func TestTamperedEnvelopesDoNotOpenNorMoveTheChain(t *testing.T) {
	admin := newAuthor(t, "aa")
	e := epochOf(t, admin, 1)
	alice, bob := NewKeyringWith(t, e, "aa"), NewKeyringWith(t, e, "bb")
	good, _ := alice.Seal([]byte("hello"))

	cases := map[string]func(v *Envelope){
		"ciphertext":  func(v *Envelope) { v.CT[0] ^= 1 },
		"sender":      func(v *Envelope) { v.Sender = "cc" },
		"index":       func(v *Envelope) { v.N = 7 },
		"nonce":       func(v *Envelope) { v.Nonce[0] ^= 1 },
		"short nonce": func(v *Envelope) { v.Nonce = v.Nonce[:5] },
	}
	for name, mutate := range cases {
		v := *good
		v.CT = append([]byte(nil), good.CT...)
		v.Nonce = append([]byte(nil), good.Nonce...)
		mutate(&v)
		if _, err := bob.Open(&v); err == nil {
			t.Fatalf("%s: a tampered envelope opened", name)
		}
	}
	// none of that moved alice's chain at bob: the real message still opens, and nothing skipped
	if p, err := bob.Open(good); err != nil || string(p) != "hello" {
		t.Fatalf("the real message: %q %v", p, err)
	}
	for _, st := range bob.Have() {
		if st.Sender == "aa" && (st.Next != 1 || len(st.Missing) != 0) {
			t.Fatalf("chain moved by junk: %+v", st)
		}
	}

	if _, err := bob.Open(nil); !errors.Is(err, ErrBadEnvelope) {
		t.Fatal(err)
	}
	other := *good
	other.Group = "g2"
	if _, err := bob.Open(&other); !errors.Is(err, ErrBadEnvelope) {
		t.Fatal(err)
	}
	unknown := *good
	unknown.Tag = "00"
	if _, err := bob.Open(&unknown); !errors.Is(err, ErrUnknownEpoch) {
		t.Fatal(err)
	}
}

func TestChainLimits(t *testing.T) {
	admin := newAuthor(t, "aa")
	e := epochOf(t, admin, 1)
	alice, bob := NewKeyringWith(t, e, "aa"), NewKeyringWith(t, e, "bb")
	first, _ := alice.Seal([]byte("0"))
	// a message far ahead of the chain is refused, not computed up to
	far := *first
	far.N = MaxSkip + 1
	if _, err := bob.Open(&far); !errors.Is(err, ErrTooFarAhead) {
		t.Fatalf("got %v", err)
	}
	// exactly MaxSkip ahead is fine
	for i := 0; i < MaxSkip; i++ {
		if _, err := alice.Seal([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	last, _ := alice.Seal([]byte("last"))
	if last.N != MaxSkip+1 {
		t.Fatalf("index %d", last.N)
	}
	if _, err := bob.Open(first); err != nil {
		t.Fatal(err)
	}
	if p, err := bob.Open(last); err != nil || string(p) != "last" {
		t.Fatalf("%q %v", p, err)
	}
	if st := bob.Have(); len(st) != 1 || len(st[0].Missing) != MaxSkip {
		t.Fatalf("missing kept: %d", len(st[0].Missing))
	}
	// trimming keeps the newest MaxSkip
	c := &chain{Skipped: map[uint32][]byte{}}
	for i := uint32(0); i < MaxSkip+10; i++ {
		c.Skipped[i] = []byte{1}
	}
	trimSkipped(c)
	if len(c.Skipped) != MaxSkip || c.Skipped[0] != nil || c.Skipped[MaxSkip+9] == nil {
		t.Fatal("trim dropped the wrong keys")
	}
}

func TestEpochOrderAndVerify(t *testing.T) {
	a := newAuthor(t, "aa")
	e1, e2a, e2b := epochOf(t, a, 1), epochOf(t, a, 2), epochOf(t, a, 2)
	if !e2a.ID().After(e1.ID()) || e1.ID().After(e2a.ID()) {
		t.Fatal("the higher number is newer")
	}
	// same number: exactly one of the two is newer, the one with the smaller tag
	if e2a.ID().After(e2b.ID()) == e2b.ID().After(e2a.ID()) {
		t.Fatal("ties are broken one way")
	}
	k := NewKeyring("g1", "aa")
	for _, e := range []*Epoch{e1, e2a, e2b} {
		k.Add(e)
	}
	want := e2a
	if e2b.ID().After(e2a.ID()) {
		want = e2b
	}
	if k.Current() != want {
		t.Fatal("current is not the newest")
	}
	if l := k.List(); l[0] != want || l[2] != e1 {
		t.Fatal("list is not newest first")
	}
	if e1.ID().String() == "" || !k.Add(epochOf(t, a, 3)) || k.Add(&Epoch{Group: "other"}) || k.Add(nil) {
		t.Fatal("add")
	}

	// verify: the signature, and the shape
	other := newAuthor(t, "bb")
	if !errors.Is(e1.Verify(other.pub), ErrBadSignature) {
		t.Fatal("another key verified")
	}
	bad := []func(e *Epoch){
		func(e *Epoch) { e.Secret = e.Secret[:16] },
		func(e *Epoch) { e.N = 0 },
		func(e *Epoch) { e.Group = "" },
		func(e *Epoch) { e.Creator = "" },
		func(e *Epoch) { e.Members = []string{"b", "a"} },
	}
	for i, mutate := range bad {
		c := *e1
		mutate(&c)
		if c.Verify(a.pub) == nil {
			t.Fatalf("case %d verified", i)
		}
	}
	var nilE *Epoch
	if nilE.Verify(a.pub) == nil || e1.Verify(nil) == nil {
		t.Fatal("nil")
	}
	changed := *e1
	changed.Members = []string{"zz"}
	if changed.Verify(a.pub) == nil {
		t.Fatal("members are signed")
	}
	if m := epochOf(t, a, 1, "cc", "bb"); !m.Has("bb") || !m.Has("cc") || m.Has("dd") {
		t.Fatal("has")
	}
}

func TestPruneAndStale(t *testing.T) {
	a := newAuthor(t, "aa")
	k := NewKeyring("g1", "aa")
	other := NewKeyring("g1", "bb")
	now := time.Now()
	var es []*Epoch
	for n := uint64(1); n <= 5; n++ {
		e := epochOf(t, a, n)
		es = append(es, e)
		k.Add(e)
		other.Add(e)
		v, _ := other.Seal([]byte("x"))
		if _, err := k.Open(v); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Seal([]byte("y")); err != nil {
			t.Fatal(err)
		}
	}
	// all made just now: a message written in any of them can still come, nothing goes
	if gone := k.Prune(now); gone != 0 {
		t.Fatalf("pruned %d fresh epochs", gone)
	}
	// epochs 1 and 2 were replaced (by 2 and 3) more than a week ago; 3 by 4 just now
	day := 24 * time.Hour
	es[0].CreatedAt = now.Add(-10 * day).UnixMilli()
	es[1].CreatedAt = now.Add(-9 * day).UnixMilli()
	es[2].CreatedAt = now.Add(-8 * day).UnixMilli()
	if gone := k.Prune(now); gone != 2 {
		t.Fatalf("pruned %d, want epochs 1 and 2", gone)
	}
	if k.Epoch(es[0].ID()) != nil || k.Epoch(es[1].ID()) != nil || k.Epoch(es[2].ID()) == nil {
		t.Fatal("the wrong epochs went")
	}
	if len(k.Epochs) != 3 || len(k.Send) != 3 || len(k.Recv) != 3 {
		t.Fatalf("left %d epochs, %d send, %d recv", len(k.Epochs), len(k.Send), len(k.Recv))
	}
	if k.Prune(now) != 0 {
		t.Fatal("prune twice")
	}
	// the current epoch is never pruned, however old
	solo := NewKeyring("g1", "aa")
	old := epochOf(t, a, 1)
	old.CreatedAt = now.Add(-100 * day).UnixMilli()
	solo.Add(old)
	if solo.Prune(now) != 0 {
		t.Fatal("the only epoch went")
	}
	// and no more than MaxEpochs stay
	many := NewKeyring("g1", "aa")
	for n := uint64(1); n <= MaxEpochs+5; n++ {
		many.Add(epochOf(t, a, n))
	}
	if gone := many.Prune(now); gone != 5 || len(many.Epochs) != MaxEpochs {
		t.Fatalf("pruned %d, kept %d", gone, len(many.Epochs))
	}

	if k.Stale(now) || !k.Stale(now.Add(MaxEpochAge+time.Minute)) {
		t.Fatal("stale")
	}
	if NewKeyring("g", "x").Stale(now) {
		t.Fatal("no epoch is not stale")
	}
}

func TestHaveAndWants(t *testing.T) {
	a := newAuthor(t, "aa")
	e := epochOf(t, a, 1)
	alice, bob := NewKeyringWith(t, e, "aa"), NewKeyringWith(t, e, "bb")
	var envs []*Envelope
	for i := 0; i < 4; i++ {
		v, _ := alice.Seal([]byte("x"))
		envs = append(envs, v)
	}
	_, _ = bob.Open(envs[0])
	_, _ = bob.Open(envs[2]) // 1 missing
	have := bob.Have()
	epochs := []EpochID{e.ID()}
	if len(have) != 1 || have[0].Next != 3 || len(have[0].Missing) != 1 || have[0].Missing[0] != 1 {
		t.Fatalf("have: %+v", have)
	}
	for n, want := range map[uint32]bool{0: false, 1: true, 2: false, 3: true, 9: true} {
		if Wants(have, epochs, e.ID(), "aa", n) != want {
			t.Fatalf("message %d: want %v", n, want)
		}
	}
	if !Wants(have, epochs, e.ID(), "cc", 0) {
		t.Fatal("a sender not heard of yet is wanted")
	}
	if Wants(have, nil, e.ID(), "aa", 3) {
		t.Fatal("an epoch it does not hold is not wanted")
	}
	// a chain with an epoch the keyring no longer holds is left out of Have
	bob.Recv["9.ff|aa"] = &chain{Key: []byte{1}}
	bob.Recv["broken"] = &chain{Key: []byte{1}}
	if len(bob.Have()) != 1 {
		t.Fatal("have lists chains of unknown epochs")
	}
}

func TestContentSignature(t *testing.T) {
	a, b := newAuthor(t, "aa"), newAuthor(t, "bb")
	c := Content{Group: "g1", ID: "m1", TS: 5, Sender: "aa", Text: "hi", Extra: []byte(`{"file":1}`)}
	if err := c.Sign(a.sign); err != nil || c.FileDigest == "" {
		t.Fatal(err)
	}
	if err := c.Verify(a.pub); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Verify(b.pub), ErrBadSignature) || !errors.Is(c.Verify(nil), ErrBadSignature) {
		t.Fatal("another key")
	}
	// history carries the digest without the file: it still verifies
	h := c
	h.Extra = nil
	if err := h.Verify(a.pub); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Content){
		"text":   func(x *Content) { x.Text = "bye" },
		"sender": func(x *Content) { x.Sender = "bb" },
		"time":   func(x *Content) { x.TS = 6 },
		"id":     func(x *Content) { x.ID = "m2" },
		"group":  func(x *Content) { x.Group = "g2" },
		"extra":  func(x *Content) { x.Extra = []byte("other") },
	} {
		x := c
		mutate(&x)
		if x.Verify(a.pub) == nil {
			t.Fatalf("%s change verified", name)
		}
	}
	failing := func([]byte) ([]byte, error) { return nil, errors.New("no key") }
	if (&Content{}).Sign(failing) == nil {
		t.Fatal("sign error")
	}
}

type badReader struct{}

func (badReader) Read([]byte) (int, error) { return 0, errors.New("no randomness") }

func TestFailuresOfRandomnessAndSigning(t *testing.T) {
	a := newAuthor(t, "aa")
	e := epochOf(t, a, 1)
	k := NewKeyringWith(t, e, "aa")
	randReader = badReader{}
	defer func() { randReader = rand.Reader }()
	if _, err := NewEpoch("g1", 2, "aa", nil, time.Now(), a.sign); err == nil {
		t.Fatal("epoch without randomness")
	}
	if _, err := k.Seal([]byte("x")); err == nil {
		t.Fatal("seal without randomness")
	}
	randReader = rand.Reader
	// a failed seal does not use up the chain
	if v, err := k.Seal([]byte("x")); err != nil || v.N != 0 {
		t.Fatalf("after a failed seal: %v %v", v, err)
	}
	if _, err := NewEpoch("g1", 2, "aa", nil, time.Now(), func([]byte) ([]byte, error) { return nil, errors.New("x") }); err == nil {
		t.Fatal("epoch without a signature")
	}
	if _, err := NewKeyring("g1", "aa").Seal([]byte("x")); !errors.Is(err, ErrUnknownEpoch) {
		t.Fatal("seal with no epoch")
	}
}

// the keyring is saved as JSON after every change; what comes back works the same
func TestKeyringSurvivesSaving(t *testing.T) {
	a := newAuthor(t, "aa")
	e := epochOf(t, a, 1)
	alice, bob := NewKeyringWith(t, e, "aa"), NewKeyringWith(t, e, "bb")
	v0, _ := alice.Seal([]byte("0"))
	v1, _ := alice.Seal([]byte("1"))
	v2, _ := alice.Seal([]byte("2"))
	_, _ = bob.Open(v0)
	_, _ = bob.Open(v2) // 1 skipped

	save := func(k *Keyring) *Keyring {
		raw, err := json.Marshal(k)
		if err != nil {
			t.Fatal(err)
		}
		var out Keyring
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return &out
	}
	bob2, alice2 := save(bob), save(alice)
	if p, err := bob2.Open(v1); err != nil || string(p) != "1" {
		t.Fatalf("skipped key after saving: %q %v", p, err)
	}
	v3, _ := alice2.Seal([]byte("3"))
	if v3.N != 3 {
		t.Fatalf("sending chain after saving at %d", v3.N)
	}
	if p, err := bob2.Open(v3); err != nil || string(p) != "3" {
		t.Fatal(err)
	}
	// a zero keyring (as unmarshalled from an older record) is usable
	var empty Keyring
	empty.Group = "g1"
	empty.Self = "cc"
	if !empty.Add(e) {
		t.Fatal("add to a zero keyring")
	}
	var zero Keyring
	zero.Group, zero.Self = "g1", "dd"
	if _, err := zero.Seal(nil); !errors.Is(err, ErrUnknownEpoch) {
		t.Fatal(err)
	}
	if _, err := (&Keyring{Group: "g1"}).Open(v3); !errors.Is(err, ErrUnknownEpoch) {
		t.Fatal(err)
	}
}

func TestDifferentEpochsAndSendersGiveDifferentKeys(t *testing.T) {
	a := newAuthor(t, "aa")
	e1, e2 := epochOf(t, a, 1), epochOf(t, a, 1)
	if bytes.Equal(chainStart(e1, "aa"), chainStart(e2, "aa")) || bytes.Equal(chainStart(e1, "aa"), chainStart(e1, "bb")) {
		t.Fatal("chains collide")
	}
	mk, next := step(chainStart(e1, "aa"))
	if bytes.Equal(mk, next) {
		t.Fatal("message key equals the next chain")
	}
	// an envelope of one epoch does not open in another with the same number
	k1, k2 := NewKeyringWith(t, e1, "aa"), NewKeyringWith(t, e2, "bb")
	v, _ := k1.Seal([]byte("x"))
	v.Tag = e2.ID().Tag
	if _, err := k2.Open(v); err == nil {
		t.Fatal("opened under another epoch")
	}
}
