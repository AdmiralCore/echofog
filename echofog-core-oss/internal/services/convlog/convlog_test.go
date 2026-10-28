package convlog

import (
	"crypto/ed25519"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

type account struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	id   types.PeerID
}

func newAccount(t *testing.T) account {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	return account{pub: pub, priv: priv, id: types.PeerPubKeyToID(pk)}
}

func memStore(t *testing.T) storage.Storage {
	t.Helper()
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	st := storage.NewBadgerStorage(db)
	t.Cleanup(st.Close)
	return st
}

// replica is one device of an account.
func replica(t *testing.T, a account) *Log {
	return New(memStore(t), a.pub, func(b []byte) ([]byte, error) { return ed25519.Sign(a.priv, b), nil })
}

func ingest(t *testing.T, l *Log, c Conv, raws ...[]byte) {
	t.Helper()
	for _, r := range raws {
		if _, _, err := l.Ingest(c, r); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
}

func TestEventSignatureAndID(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la := replica(t, a)
	c := DM(a.id, b.id)
	raw, id := mustF(t)(la.AppendMessage(c, "hello", 0, "", EventID{}, EventID{}))
	e, id2, err := Decode(raw)
	if err != nil || id2 != id {
		t.Fatalf("decode: %v, id %s vs %s", err, id2, id)
	}
	// any change breaks the signature
	tampered := proto.Clone(e).(*Event)
	tampered.Lamport++
	out, _ := Encode(tampered)
	if _, _, err := Decode(out); err == nil {
		t.Fatal("a tampered event verified")
	}
	// a stripped event keeps its id and signature
	s, _ := Encode(Strip(e))
	se, sid, err := Decode(s)
	if err != nil || sid != id || !Stripped(se) {
		t.Fatalf("stripped: err=%v same id=%v stripped=%v", err, sid == id, Stripped(se))
	}
	if strings.Contains(string(s), "hello") {
		t.Fatal("the stripped event still carries the text")
	}
}

// mustF adapts the (raw, id, err) results of the Append helpers.
func mustF(t *testing.T) func([]byte, EventID, error) ([]byte, EventID) {
	return func(raw []byte, id EventID, err error) ([]byte, EventID) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return raw, id
	}
}

func TestDirectChatOperations(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	if ca.ID != cb.ID {
		t.Fatal("the two sides compute different conversation ids")
	}
	m := mustF(t)
	r1, id1 := m(la.AppendMessage(ca, "first", 0, "", EventID{}, EventID{}))
	ingest(t, lb, cb, r1)

	// Bob answers, reacts and pins; Alice edits
	r2, _ := m(lb.AppendMessage(cb, "reply", 0, "", id1, EventID{}))
	r3, _ := m(lb.AppendReact(cb, id1, "👍", true))
	r4, _ := m(lb.AppendPin(cb, id1, true))
	r5, _ := m(la.AppendEdit(ca, id1, "first (edited)"))
	ingest(t, la, ca, r2, r3, r4)
	ingest(t, lb, cb, r5)

	for name, l := range map[string]*Log{"alice": la, "bob": lb} {
		msg, ok := l.Message(ca, id1)
		if !ok {
			t.Fatalf("%s: message missing", name)
		}
		if msg.Text != "first (edited)" || !msg.Edited || !msg.Pinned || len(msg.Reactions) != 1 || msg.Reactions[0].Count != 1 {
			t.Fatalf("%s: %+v", name, msg)
		}
		page, _, err := l.History(ca, "", 10)
		if err != nil || len(page) != 2 || page[0].ReplyTo != id1.String() {
			t.Fatalf("%s: history %+v err %v", name, page, err)
		}
		pins, _ := l.Pins(ca)
		if len(pins) != 1 || pins[0].ID != id1.String() {
			t.Fatalf("%s: pins %+v", name, pins)
		}
	}

	// Bob may not edit or delete Alice's message
	if _, _, err := lb.AppendEdit(cb, id1, "hijack"); err != ErrForbidden {
		t.Fatalf("edit by a non-author: %v", err)
	}
	if _, _, err := lb.AppendDelete(cb, id1); err != ErrForbidden {
		t.Fatalf("delete by a non-author: %v", err)
	}

	// Alice deletes it for everyone: the content is gone from Bob's storage too
	r6, _ := m(la.AppendDelete(ca, id1))
	ingest(t, lb, cb, r6)
	msg, _ := lb.Message(cb, id1)
	if !msg.Deleted || msg.Text != "" || msg.Pinned {
		t.Fatalf("after delete: %+v", msg)
	}
	if pins, _ := lb.Pins(cb); len(pins) != 0 {
		t.Fatal("a deleted message is still pinned")
	}
	entries, _ := lb.st.FindValues([]byte(pfxEvent), 0)
	for _, e := range entries {
		if strings.Contains(string(e.Value), "first") {
			t.Fatal("the deleted text (or its edit) is still stored")
		}
	}
}

// buildScenario makes a set of events from two authors with real causal order, and returns
// them with the id of the message they act on.
func buildScenario(t *testing.T, a, b account, withDelete bool) ([][]byte, EventID) {
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	m := mustF(t)
	var all [][]byte
	r, target := m(la.AppendMessage(ca, "v1", 0, "", EventID{}, EventID{}))
	all = append(all, r)
	ingest(t, lb, cb, r)
	steps := []func() []byte{
		func() []byte { r, _ := m(lb.AppendReact(cb, target, "👍", true)); return r },
		func() []byte { r, _ := m(la.AppendEdit(ca, target, "v2")); return r },
		func() []byte { r, _ := m(lb.AppendReact(cb, target, "🔥", true)); return r },
		func() []byte { r, _ := m(lb.AppendReact(cb, target, "👍", false)); return r },
		func() []byte { r, _ := m(la.AppendEdit(ca, target, "v3")); return r },
		func() []byte { r, _ := m(lb.AppendPin(cb, target, true)); return r },
		func() []byte { r, _ := m(la.AppendReact(ca, target, "🔥", true)); return r },
		func() []byte { r, _ := m(lb.AppendMessage(cb, "in thread", 0, "", EventID{}, target)); return r },
	}
	if withDelete {
		steps = append(steps, func() []byte { r, _ := m(la.AppendDelete(ca, target)); return r })
	}
	for i, step := range steps {
		r := step()
		all = append(all, r)
		// the author's partner sees it before the next step, so clocks move forward
		if i%2 == 0 {
			ingest(t, la, ca, r)
		} else {
			ingest(t, lb, cb, r)
		}
		// make both fully caught up on everything so far
		for _, x := range all {
			_, _, _ = la.Ingest(ca, x)
			_, _, _ = lb.Ingest(cb, x)
		}
	}
	return all, target
}

func renderJSON(t *testing.T, l *Log, c Conv, id EventID) string {
	msg, ok := l.Message(c, id)
	if !ok {
		t.Fatal("message missing")
	}
	msg.Mine = false
	raw, _ := json.Marshal(msg)
	return string(raw)
}

// TestOrderDoesNotMatter applies the same events in many orders: every replica ends the same.
func TestOrderDoesNotMatter(t *testing.T) {
	for _, withDelete := range []bool{false, true} {
		a, b := newAccount(t), newAccount(t)
		events, target := buildScenario(t, a, b, withDelete)
		c := DM(b.id, a.id)
		rng := rand.New(rand.NewSource(7))
		want := ""
		for i := 0; i < 25; i++ {
			perm := rng.Perm(len(events))
			l := replica(t, b)
			for _, p := range perm {
				if _, _, err := l.Ingest(c, events[p]); err != nil {
					t.Fatalf("ingest: %v", err)
				}
			}
			got := renderJSON(t, l, c, target)
			if i == 0 {
				want = got
				continue
			}
			if got != want {
				t.Fatalf("delete=%v order %v gave\n%s\nwant\n%s", withDelete, perm, got, want)
			}
		}
		var msg Message
		_ = json.Unmarshal([]byte(want), &msg)
		if withDelete {
			if !msg.Deleted || msg.Text != "" {
				t.Fatalf("deleted: %+v", msg)
			}
		} else if msg.Text != "v3" || !msg.Pinned || msg.Replies != 1 || len(msg.Reactions) != 1 || msg.Reactions[0].Emoji != "🔥" || msg.Reactions[0].Count != 2 {
			t.Fatalf("merged: %+v", msg)
		}
	}
}

func TestThreadsStayOutOfTheMainHistory(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la := replica(t, a)
	c := DM(a.id, b.id)
	m := mustF(t)
	_, root := m(la.AppendMessage(c, "post", 0, "", EventID{}, EventID{}))
	for i := 0; i < 3; i++ {
		m(la.AppendMessage(c, "comment", 0, "", EventID{}, root))
	}
	main, _, _ := la.History(c, "", 10)
	if len(main) != 1 || main[0].Replies != 3 {
		t.Fatalf("main history: %+v", main)
	}
	thread, _, _ := la.Thread(c, root, "", 10)
	if len(thread) != 3 || thread[0].Thread != root.String() {
		t.Fatalf("thread: %+v", thread)
	}
}

func TestHistoryPages(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la := replica(t, a)
	c := DM(a.id, b.id)
	for i := 0; i < 25; i++ {
		mustF(t)(la.AppendMessage(c, "m", 0, "", EventID{}, EventID{}))
	}
	seen := map[string]bool{}
	cursor, last := "", ^uint64(0)
	for pages := 0; ; pages++ {
		page, next, err := la.History(c, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range page {
			if seen[msg.ID] || msg.Lamport >= last {
				t.Fatalf("page order broken at %s", msg.ID)
			}
			seen[msg.ID], last = true, msg.Lamport
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 25 {
		t.Fatalf("read %d of 25", len(seen))
	}
}

func TestUnreadAndMarkRead(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	var last EventID
	for i := 0; i < 4; i++ {
		var r []byte
		r, last = mustF(t)(la.AppendMessage(ca, "x", 0, "", EventID{}, EventID{}))
		ingest(t, lb, cb, r)
	}
	if meta, _ := lb.Meta(cb); meta.Unread != 4 {
		t.Fatalf("unread %d", meta.Unread)
	}
	if err := lb.MarkRead(cb, last); err != nil {
		t.Fatal(err)
	}
	if meta, _ := lb.Meta(cb); meta.Unread != 0 {
		t.Fatalf("unread after reading %d", meta.Unread)
	}
	// my own message reads everything before it
	r, _ := mustF(t)(la.AppendMessage(ca, "y", 0, "", EventID{}, EventID{}))
	ingest(t, lb, cb, r)
	mustF(t)(lb.AppendMessage(cb, "answer", 0, "", EventID{}, EventID{}))
	if meta, _ := lb.Meta(cb); meta.Unread != 0 {
		t.Fatalf("unread after answering %d", meta.Unread)
	}
}

// A message written without having seen mine (at the same time, or while we were apart) carries a
// clock no later than my last message, yet it is new to me: it counts as unread until I read.
func TestAMessageThatCrossedMineIsUnread(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	mustF(t)(lb.AppendMessage(cb, "mine", 0, "", EventID{}, EventID{}))
	mustF(t)(lb.AppendMessage(cb, "mine too", 0, "", EventID{}, EventID{}))
	theirs, _ := mustF(t)(la.AppendMessage(ca, "written before seeing yours", 0, "", EventID{}, EventID{}))
	ingest(t, lb, cb, theirs)
	if meta, _ := lb.Meta(cb); meta.Unread != 1 {
		t.Fatalf("unread %d, want 1", meta.Unread)
	}
	page, _, err := lb.History(cb, "", 10)
	if err != nil || len(page) != 3 {
		t.Fatalf("history: %v, %d messages", err, len(page))
	}
	if err := lb.MarkRead(cb, mustID(t, page[0].ID)); err != nil {
		t.Fatal(err)
	}
	if meta, _ := lb.Meta(cb); meta.Unread != 0 {
		t.Fatalf("unread after reading to the end %d", meta.Unread)
	}
}

func mustID(t *testing.T, s string) EventID {
	t.Helper()
	id, err := ParseEventID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStrangersAndClockSpamAreRejected(t *testing.T) {
	a, b, x := newAccount(t), newAccount(t), newAccount(t)
	lb := replica(t, b)
	cb := DM(b.id, a.id)
	forge := func(by account, lamport uint64) []byte {
		e := &Event{Conv: cb.ID[:], Author: by.pub, Seq: 1, Lamport: lamport, TsMs: 1, Kind: KindMsg,
			Body: encodeBody(&MsgBody{Text: "spam"})}
		if err := Sign(e, func(m []byte) ([]byte, error) { return ed25519.Sign(by.priv, m), nil }); err != nil {
			t.Fatal(err)
		}
		raw, _ := Encode(e)
		return raw
	}
	// an outsider writing into a's and b's conversation, with a valid signature of its own
	if _, _, err := lb.Ingest(cb, forge(x, 1)); err != ErrNotParticipant {
		t.Fatalf("outsider accepted: %v", err)
	}
	// a participant pushing the clock absurdly far
	if _, _, err := lb.Ingest(cb, forge(a, 1<<40)); err != ErrClock {
		t.Fatalf("clock spam: %v", err)
	}
}

func TestEquivocationKeepsBoth(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	lb := replica(t, b)
	c := DM(b.id, a.id)
	sign := func(m []byte) ([]byte, error) { return ed25519.Sign(a.priv, m), nil }
	mk := func(text string) []byte {
		e := &Event{Conv: c.ID[:], Author: a.pub, Seq: 1, Lamport: 1, TsMs: 1, Kind: KindMsg,
			Body: encodeBody(&MsgBody{Text: text})}
		_ = Sign(e, sign)
		raw, _ := Encode(e)
		return raw
	}
	ingest(t, lb, c, mk("one"), mk("two"))
	page, _, _ := lb.History(c, "", 10)
	meta, _ := lb.Meta(c)
	if len(page) != 2 || meta.Forks[peerHex(a.id)] != 1 {
		t.Fatalf("history %d, forks %v", len(page), meta.Forks)
	}
}

// pump runs sync messages between two replicas until they are quiet.
func pump(t *testing.T, x, y *Log, xid, yid types.PeerID, c Conv) int {
	t.Helper()
	type msg struct {
		to   *Log
		from types.PeerID
		m    *Sync
	}
	queue := []msg{{y, xid, x.Hello([]Conv{c})}, {x, yid, y.Hello([]Conv{DM(yid, xid)})}}
	rounds := 0
	for len(queue) > 0 {
		rounds++
		if rounds > 10000 {
			t.Fatal("sync does not end")
		}
		q := queue[0]
		queue = queue[1:]
		raw, err := EncodeSync(q.m)
		if err != nil {
			t.Fatal(err)
		}
		m, err := DecodeSync(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range q.to.HandleSync(q.from, m, nil) {
			back := x
			if q.to == x {
				back = y
			}
			var self types.PeerID
			if q.to == x {
				self = xid
			} else {
				self = yid
			}
			queue = append(queue, msg{back, self, r})
		}
	}
	return rounds
}

func TestSyncCatchesUpBothWays(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	m := mustF(t)
	// both wrote while apart; Alice's messages are big enough to need several answers (> 2 MB)
	big := strings.Repeat("x", MaxTextLen-64)
	var lastA EventID
	for i := 0; i < 150; i++ {
		_, lastA = m(la.AppendMessage(ca, big, 0, "", EventID{}, EventID{}))
	}
	for i := 0; i < 5; i++ {
		m(lb.AppendMessage(cb, "from bob", 0, "", EventID{}, EventID{}))
	}
	m(lb.AppendReact(cb, lastA, "👀", true)) // an op on a message Bob has not got yet
	pump(t, la, lb, a.id, b.id, ca)

	ma, _ := la.Meta(ca)
	mb, _ := lb.Meta(cb)
	if string(ma.Digest()) != string(mb.Digest()) {
		t.Fatalf("not in sync: %v vs %v", ma.VV, mb.VV)
	}
	if ma.VV[peerHex(a.id)] != 150 || ma.VV[peerHex(b.id)] != 6 {
		t.Fatalf("vector %v", ma.VV)
	}
	msg, ok := lb.Message(cb, lastA)
	if !ok || len(msg.Reactions) != 1 {
		t.Fatalf("the early reaction did not land: %+v", msg)
	}
	// once in sync, a hello is all it takes
	if r := pump(t, la, lb, a.id, b.id, ca); r != 2 {
		t.Fatalf("in-sync exchange took %d messages", r)
	}
}

func TestWithheldBodyIsUpgraded(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	la, lb := replica(t, a), replica(t, b)
	ca, cb := DM(a.id, b.id), DM(b.id, a.id)
	raw, id := mustF(t)(la.AppendMessage(ca, "secret", 0, "", EventID{}, EventID{}))
	e, _, _ := Decode(raw)
	stripped, _ := Encode(Strip(e))
	ingest(t, lb, cb, stripped)
	if msg, _ := lb.Message(cb, id); !msg.Withheld || msg.Text != "" {
		t.Fatalf("stripped copy: %+v", msg)
	}
	ingest(t, lb, cb, raw)
	if msg, _ := lb.Message(cb, id); msg.Withheld || msg.Text != "secret" {
		t.Fatalf("after the full copy: %+v", msg)
	}
}

func TestSealedStoreWorks(t *testing.T) {
	a, b := newAccount(t), newAccount(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	sealed, err := storage.NewSealed(memStore(t), memguard.NewEnclave(key))
	if err != nil {
		t.Fatal(err)
	}
	l := New(sealed, a.pub, func(m []byte) ([]byte, error) { return ed25519.Sign(a.priv, m), nil })
	c := DM(a.id, b.id)
	_, id := mustF(t)(l.AppendMessage(c, "sealed text", 0, "", EventID{}, EventID{}))
	if msg, ok := l.Message(c, id); !ok || msg.Text != "sealed text" {
		t.Fatalf("read back: %+v", msg)
	}
	// nothing readable underneath
	raw, _ := sealed.Inner().FindValues([]byte("cl/"), 0)
	for _, e := range raw {
		if strings.Contains(string(e.Value), "sealed text") || strings.Contains(string(e.Key), c.ID.String()) {
			t.Fatal("conversation data is stored in the open")
		}
	}
}

// A message says what it is: the two file formats come out as one, an invitation to a call as a
// call, an app line as a system line.
func TestMessageKinds(t *testing.T) {
	cases := []struct {
		typ        uint32
		text, meta string
		kind       string
		file       *FileRef
	}{
		{0, "hello", "", "text", nil},
		{0, CallInvitePrefix + "abc", "", "call", nil},
		{MsgTypeSystem, "joined", "", "system", nil},
		{4, "a.pdf", `{"file_name":"a.pdf","file_size":1200,"mime_type":"application/pdf","custom":{"fid":"f1"}}`, "file",
			&FileRef{ID: "f1", Name: "a.pdf", Size: 1200, Mime: "application/pdf"}},
		{0, "b.png", `{"group_file":{"id":"g1","name":"b.png","size":"77","mime":"image/png"}}`, "file",
			&FileRef{ID: "g1", Name: "b.png", Size: 77, Mime: "image/png"}},
		{4, "c.jpg", `{"file_name":"c.jpg","file_size":9,"custom":{"fid":"f2","thumb":"QUJD","tw":192,"th":128}}`, "file",
			&FileRef{ID: "f2", Name: "c.jpg", Size: 9, Thumb: "QUJD", W: 192, H: 128}},
	}
	for _, c := range cases {
		v := View{Present: true, Type: c.typ, Text: c.text, Metadata: c.meta}
		m := v.Render("conv", "me")
		if m.Kind != c.kind || (c.file == nil) != (m.File == nil) || (c.file != nil && *c.file != *m.File) {
			t.Errorf("%q: kind %q file %+v", c.text, m.Kind, m.File)
		}
	}
}
