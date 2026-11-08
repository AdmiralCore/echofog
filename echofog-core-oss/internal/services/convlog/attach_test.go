package convlog

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// ---- T11.3e: a file in a group is the attachment of a message, a group is closed by its owner --------------------

func goodAtt() *Attachment {
	fid := bytes.Repeat([]byte{0x11}, 16)
	return &Attachment{Fid: fid, Size: 4096, MerkleRoot: bytes.Repeat([]byte{0x22}, 32), Name: "схема.pdf", Mime: "application/pdf"}
}

func fidOf(a *Attachment) [16]byte { return [16]byte(a.Fid) }

// forgeEvent signs an event of kind with body as author a, without asking a's log (which would refuse).
func forgeEvent(t *testing.T, a account, c Conv, kind uint32, seq, lamport uint64, body, ext []byte) []byte {
	t.Helper()
	e := &Event{Conv: c.ID[:], Author: a.pub, Seq: seq, Lamport: lamport, TsMs: 1, Kind: kind, Body: body, Ext: ext}
	if err := Sign(e, func(m []byte) ([]byte, error) { return signWith(a, m), nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type team struct {
	f, x, y account
	lf, lx  *Log
	c       Conv
}

// newTeam: an owner f, a member x, and a replica of each; y is a stranger.
func newTeam(t *testing.T, rules testRules) *team {
	t.Helper()
	f, x, y := newAccount(t), newAccount(t), newAccount(t)
	rules.founder = f.id
	c := Group("g-att")
	tm := &team{f: f, x: x, y: y, c: c, lf: groupReplica(t, f, rules), lx: groupReplica(t, x, rules)}
	r := member(t, tm.lf, c, roster(f, "owner", x, "member"))
	ingest(t, tm.lx, c, r)
	return tm
}

func TestAttachmentTravelsSignedAndShowsAsAFileCard(t *testing.T) {
	tm := newTeam(t, testRules{})
	a := goodAtt()
	raw, id, err := tm.lx.AppendAttachment(tm.c, "", a, "вот схема", EventID{}, EventID{})
	if err != nil {
		t.Fatalf("AppendAttachment: %v", err)
	}
	ingest(t, tm.lf, tm.c, raw)
	for name, l := range map[string]*Log{"author": tm.lx, "owner": tm.lf} {
		ref, ok := l.AttachmentIn(tm.c, fidOf(a))
		if !ok {
			t.Fatalf("%s: the attachment is not found", name)
		}
		m := ref.Msg
		if m.ID != id.String() || m.Kind != "file" || m.File == nil || m.File.ID != hex.EncodeToString(a.Fid) || m.File.Name != "схема.pdf" ||
			m.File.Size != 4096 || m.File.Mime != "application/pdf" || m.File.Root != hex.EncodeToString(a.MerkleRoot) || m.Text != "вот схема" {
			t.Fatalf("%s: message %+v file %+v", name, m, m.File)
		}
		if ref.Root != [32]byte(a.MerkleRoot) {
			t.Fatalf("%s: root %x", name, ref.Root)
		}
	}
	// a changed byte of the attachment breaks the signature: the file id, root or name cannot be swapped by a relay
	e, _, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(e.Body, a.Fid)
	if at < 0 {
		t.Fatal("fid not in the body")
	}
	e.Body = append([]byte(nil), e.Body...)
	e.Body[at] ^= 0x01
	bad, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tm.lf.Ingest(tm.c, bad); err == nil {
		t.Fatal("an event with another file id was taken")
	}
}

func TestAttachmentIsChecked(t *testing.T) {
	tm := newTeam(t, testRules{})
	bad := map[string]func(*Attachment){
		"short fid":      func(a *Attachment) { a.Fid = a.Fid[:8] },
		"no size":        func(a *Attachment) { a.Size = 0 },
		"short root":     func(a *Attachment) { a.MerkleRoot = a.MerkleRoot[:31] },
		"empty name":     func(a *Attachment) { a.Name = "" },
		"long name":      func(a *Attachment) { a.Name = strings.Repeat("я", 128) }, // 256 bytes
		"bad utf-8 name": func(a *Attachment) { a.Name = "a\xffb" },
		"long mime":      func(a *Attachment) { a.Mime = strings.Repeat("a", 128) },
		"non-ASCII mime": func(a *Attachment) { a.Mime = "текст/план" },
		"name with path": func(a *Attachment) { a.Name = "../x" },
	}
	for name, mut := range bad {
		a := goodAtt()
		mut(a)
		if _, _, err := tm.lx.AppendAttachment(tm.c, "", a, "", EventID{}, EventID{}); !errors.Is(err, ErrBadBody) {
			t.Errorf("%s: written: %v", name, err)
		}
		// the same from the wire: kept without effect, not shown
		raw := forgeEvent(t, tm.x, tm.c, KindMsg, 1, 10, encodeBody(&MsgBody{Text: "x", Att: a}), nil)
		_, _, _ = tm.lf.Ingest(tm.c, raw)
		if _, ok := tm.lf.AttachmentIn(tm.c, [16]byte(append(append([]byte(nil), a.Fid...), make([]byte, 16)...)[:16])); ok {
			t.Errorf("%s: shown", name)
		}
	}
	if m := texts(t, tm.lf, tm.c); m["x"] {
		t.Errorf("a message with a bad attachment is shown: %v", m)
	}
}

// the receiver's rule (services.md §8): the event is in the log of this conversation, its author takes part and may post a file there
func TestAttachmentIsFoundOnlyWhereTheLogHoldsItAndTheAuthorMayPost(t *testing.T) {
	tm := newTeam(t, testRules{})
	a := goodAtt()
	raw, _, _ := tm.lx.AppendAttachment(tm.c, "", a, "", EventID{}, EventID{})
	ingest(t, tm.lf, tm.c, raw)

	// another file id
	other := goodAtt()
	other.Fid = bytes.Repeat([]byte{0x33}, 16)
	if _, ok := tm.lf.AttachmentIn(tm.c, fidOf(other)); ok {
		t.Error("a file id the log does not hold was found")
	}
	// another conversation
	if _, ok := tm.lf.AttachmentIn(Group("elsewhere"), fidOf(a)); ok {
		t.Error("found in a conversation that does not hold it")
	}
	// an author who is not in the conversation: the event is refused (nobody vouches for them)
	ly := groupReplica(t, tm.y, testRules{founder: tm.f.id})
	stranger := forgeEvent(t, tm.y, tm.c, KindMsg, 1, 20, encodeBody(&MsgBody{Text: "из ниоткуда", Att: other}), nil)
	_, _, _ = tm.lf.Ingest(tm.c, stranger)
	if _, ok := tm.lf.AttachmentIn(tm.c, fidOf(other)); ok {
		t.Error("the attachment of a stranger was found")
	}
	_ = ly
	// the author may not send files: the role has no media right
	rules := testRules{founder: tm.f.id}
	lf := groupReplica(t, tm.f, rules)
	subscriber := member(t, lf, tm.c, roster(tm.f, "owner", tm.x, "subscriber"))
	_ = subscriber
	forged := forgeEvent(t, tm.x, tm.c, KindMsg, 1, 30, encodeBody(&MsgBody{Text: "файл", Att: other}), nil)
	_, _, _ = lf.Ingest(tm.c, forged)
	if _, ok := lf.AttachmentIn(tm.c, fidOf(other)); ok {
		t.Error("the attachment of a member who may not post was found")
	}
	// deleted by its author: gone
	if _, _, err := tm.lx.AppendDelete(tm.c, mustEvent(t, raw)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := tm.lx.AttachmentIn(tm.c, fidOf(a)); ok {
		t.Error("a deleted message still offers its file")
	}
}

func mustEvent(t *testing.T, raw []byte) EventID {
	t.Helper()
	_, id, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAttachmentInAChannelNeedsTheRightToPostFilesThere(t *testing.T) {
	tm := newTeam(t, testRules{})
	if _, _, err := tm.lf.AppendChannel(tm.c, ChannelSpec{ID: "news", Name: "news", ReadOnly: true}); err != nil {
		t.Fatalf("AppendChannel: %v", err)
	}
	// the channel reaches x, who is only a member
	for _, r := range tm.lf.Export(tm.c, 1<<20) {
		_, _, _ = tm.lx.Ingest(tm.c, r)
	}
	a := goodAtt()
	if _, _, err := tm.lx.AppendAttachment(tm.c, "news", a, "", EventID{}, EventID{}); err == nil {
		t.Fatal("a member wrote a file into a read-only channel")
	}
	forged := forgeEvent(t, tm.x, tm.c, KindMsg, 1, 40, encodeBody(&MsgBody{Text: "в новости", Att: a}), extFor("news"))
	_, _, _ = tm.lf.Ingest(tm.c, forged)
	if _, ok := tm.lf.AttachmentIn(tm.c, fidOf(a)); ok {
		t.Error("a file from a read-only channel was taken from a member")
	}
	// the owner may
	b := goodAtt()
	b.Fid = bytes.Repeat([]byte{0x44}, 16)
	raw, _, err := tm.lf.AppendAttachment(tm.c, "news", b, "", EventID{}, EventID{})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	ingest(t, tm.lx, tm.c, raw)
	ref, ok := tm.lx.AttachmentIn(tm.c, fidOf(b))
	if !ok || ref.Msg.Channel != "news" {
		t.Fatalf("the owner's file in the channel: %+v %v", ref.Msg, ok)
	}
}

// ---- group.close ---------------------------------------------------------------------------------------------------

func closeVector(t *testing.T, raw []byte) map[string]uint64 {
	t.Helper()
	e, _, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindClose {
		t.Fatalf("kind %d", e.Kind)
	}
	var b CloseBody
	if err := proto.Unmarshal(e.Body, &b); err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	prev := ""
	for _, en := range b.Last {
		k := hex.EncodeToString(en.Author)
		if k <= prev {
			t.Fatalf("the vector is not sorted: %s after %s", k, prev)
		}
		prev, out[k] = k, en.Seq
	}
	return out
}

func TestOnlyTheOwnerClosesAGroupAndTheVectorNamesTheSeenSeqs(t *testing.T) {
	tm := newTeam(t, testRules{})
	hi, _ := say(t, tm.lx, tm.c, "привет")
	ingest(t, tm.lf, tm.c, hi)
	if _, _, err := tm.lx.AppendClose(tm.c); err == nil {
		t.Fatal("a member closed the group")
	}
	// a member's forged close is kept but does nothing
	forged := forgeEvent(t, tm.x, tm.c, KindClose, 2, 50, nil, nil)
	_, _, _ = tm.lf.Ingest(tm.c, forged)
	if tm.lf.Closed(tm.c) {
		t.Fatal("a member's close closed the group")
	}
	raw, _, err := tm.lf.AppendClose(tm.c)
	if err != nil {
		t.Fatalf("AppendClose: %v", err)
	}
	vec := closeVector(t, raw)
	// x wrote seq 1 and the owner holds the forged close at seq 2 (kept, without effect): the vector is what is held
	if vec[hex.EncodeToString(tm.x.pub)] != 2 || vec[hex.EncodeToString(tm.f.pub)] != 1 || len(vec) != 2 {
		t.Fatalf("vector %v", vec)
	}
	if !tm.lf.Closed(tm.c) {
		t.Fatal("the owner's own log does not know it is closed")
	}
	ingest(t, tm.lx, tm.c, raw)
	if !tm.lx.Closed(tm.c) {
		t.Fatal("the member's log does not know the group is closed")
	}
	// a direct chat is not closed
	if _, _, err := tm.lf.AppendClose(DM(tm.f.id, tm.x.id)); err == nil {
		t.Fatal("a direct chat was closed")
	}
}

func TestAfterTheCloseOnlyEventsWithinTheVectorAreTaken(t *testing.T) {
	tm := newTeam(t, testRules{})
	hi, _ := say(t, tm.lx, tm.c, "до закрытия") // x seq 1
	ingest(t, tm.lf, tm.c, hi)
	late := forgeNext(t, tm.x, tm.c, 2, 90, "x: после закрытия") // x seq 2: the owner has not seen it
	raw, _, err := tm.lf.AppendClose(tm.c)
	if err != nil {
		t.Fatal(err)
	}
	// a replica gets the close first, then the events
	ly := groupReplica(t, tm.y, testRules{founder: tm.f.id})
	roster0 := tm.lf.Export(tm.c, 1<<20)
	for _, r := range roster0 {
		_, _, _ = ly.Ingest(tm.c, r)
	}
	_, _, _ = ly.Ingest(tm.c, late)
	if !texts(t, ly, tm.c)["x: после закрытия"] && !ly.Closed(tm.c) {
		// the event sits before the close in the order of that replica: it is shown until the close arrives
		t.Log("shown before the close")
	}
	_, _, _ = ly.Ingest(tm.c, raw)
	ly.RebuildIfNeeded(tm.c)
	got := texts(t, ly, tm.c)
	if got["x: после закрытия"] || !got["до закрытия"] {
		t.Fatalf("after the close: %v", got)
	}
	if !ly.Closed(tm.c) {
		t.Fatal("not closed")
	}
	// an event beyond the vector arriving after the close is refused
	lz := groupReplica(t, newAccount(t), testRules{founder: tm.f.id})
	for _, r := range roster0 {
		_, _, _ = lz.Ingest(tm.c, r)
	}
	_, _, _ = lz.Ingest(tm.c, raw)
	_, _, _ = lz.Ingest(tm.c, late)
	if texts(t, lz, tm.c)["x: после закрытия"] {
		t.Fatal("an event beyond the vector was taken after the close")
	}
	// one inside the vector is taken even when it comes after the close
	lw := groupReplica(t, newAccount(t), testRules{founder: tm.f.id})
	for _, r := range roster0 {
		_, _, _ = lw.Ingest(tm.c, r)
	}
	_, _, _ = lw.Ingest(tm.c, raw)
	_, _, _ = lw.Ingest(tm.c, hi)
	if !texts(t, lw, tm.c)["до закрытия"] {
		t.Fatal("an event within the vector was refused")
	}
}

func TestAClosedGroupTakesNoJoinInvitationOrOwnWrites(t *testing.T) {
	tm := newTeam(t, testRules{})
	raw, _, _ := tm.lf.AppendClose(tm.c)
	ingest(t, tm.lx, tm.c, raw)
	// writing on a closed group is refused at once, on every replica
	if _, _, err := tm.lx.AppendMessage(tm.c, "ещё", 0, "", EventID{}, EventID{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("a member writes: %v", err)
	}
	if _, _, err := tm.lf.AppendMessage(tm.c, "и владелец", 0, "", EventID{}, EventID{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("the owner writes: %v", err)
	}
	if _, _, err := tm.lf.AppendInvite(tm.c, &InviteDef{N: strings.Repeat("a", 32), Exp: 4102444800}); err == nil {
		t.Fatal("an invitation was made in a closed group")
	}
	// a newcomer's join (an author with no entry) is refused wherever it comes
	join := forgeEvent(t, tm.y, tm.c, KindMember, 1, 70, marshalT(t, &MemberBody{Action: MemberJoin, Member: tm.y.pub}), nil)
	_, _, _ = tm.lx.Ingest(tm.c, join)
	if tm.lx.Participant(tm.c, tm.y.id) {
		t.Fatal("a newcomer joined a closed group")
	}
	// the archive is read: what was there stays
	if m := texts(t, tm.lx, tm.c); m == nil {
		t.Fatal("no history")
	}
}

func marshalT(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCloseIsIdempotentAndIrreversible(t *testing.T) {
	tm := newTeam(t, testRules{})
	raw, id, _ := tm.lf.AppendClose(tm.c)
	ingest(t, tm.lx, tm.c, raw)
	if _, _, err := tm.lx.Ingest(tm.c, raw); err != nil {
		t.Fatalf("the same close again: %v", err)
	}
	if !tm.lx.Closed(tm.c) {
		t.Fatal("not closed")
	}
	// a second, different close (a larger seq) is refused; the first stays the close
	second := forgeEvent(t, tm.f, tm.c, KindClose, 99, 200, marshalT(t, &CloseBody{}), nil)
	_, _, _ = tm.lx.Ingest(tm.c, second)
	m, _ := tm.lx.Meta(tm.c)
	if !tm.lx.Closed(tm.c) || m.Rebuild {
		t.Fatalf("closed %v rebuild %v", tm.lx.Closed(tm.c), m.Rebuild)
	}
	// the close survives a rebuild from the stored events
	tm.lx.RebuildIfNeeded(tm.c)
	if _, err := tm.lx.rebuildForTest(tm.c); err != nil {
		t.Fatal(err)
	}
	if !tm.lx.Closed(tm.c) {
		t.Fatal("a rebuild reopened the group")
	}
	_ = id
}

func TestFreshCloseBadBodiesAreRefused(t *testing.T) {
	tm := newTeam(t, testRules{})
	a, b := tm.f.pub, tm.x.pub
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	for name, body := range map[string][]byte{
		"unsorted":  marshalT(t, &CloseBody{Last: []*SeqEntry{{Author: b, Seq: 1}, {Author: a, Seq: 1}}}),
		"duplicate": marshalT(t, &CloseBody{Last: []*SeqEntry{{Author: a, Seq: 1}, {Author: a, Seq: 2}}}),
		"short key": marshalT(t, &CloseBody{Last: []*SeqEntry{{Author: a[:5], Seq: 1}}}),
		"garbage":   {0xff, 0xff},
	} {
		raw := forgeEvent(t, tm.f, tm.c, KindClose, 5, 60, body, nil)
		_, _, _ = tm.lf.Ingest(tm.c, raw)
		if tm.lf.Closed(tm.c) {
			t.Errorf("%s: closed", name)
		}
	}
}

func (l *Log) rebuildForTest(c Conv) (*Change, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rebuildLocked(c, l.bc(c.ID))
}

// A reader that does not know the attachment (versioning §3.9) reads the message as text: the text is
// field 1 of the body as ever, the attachment is field 6, which it keeps unknown without touching the
// signature. A reader that does not know kind 10 stores and forwards it (the unknown-kind tests).
func TestAttachmentLeavesTheTextOfTheBodyWhereAnOldReaderLooks(t *testing.T) {
	body := encodeBody(&MsgBody{Text: "вот схема", Att: goodAtt()})
	var text string
	var att bool
	b := body
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			t.Fatal("bad tag")
		}
		b = b[n:]
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			t.Fatal("bad field")
		}
		switch num {
		case 1:
			v, _ := protowire.ConsumeBytes(b)
			text = string(v)
		case 6:
			att = true
		}
		b = b[m:]
	}
	if text != "вот схема" || !att {
		t.Fatalf("text %q, attachment seen %v", text, att)
	}
}
