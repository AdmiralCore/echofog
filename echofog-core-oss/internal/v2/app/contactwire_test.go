package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/app"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

type sentMsg struct {
	to [32]byte
	m  *chatpb.PersonalMessageProto
}

type fakeChat struct {
	mu   sync.Mutex
	sent []sentMsg
}

func (f *fakeChat) Send(_ context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{to, m})
	return [16]byte{1}, nil
}

func (f *fakeChat) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type ingested struct {
	from [32]byte
	env  []byte
}

type wireRig struct {
	clk *tclock
	*rig
	chat *fakeChat
	got  []ingested
}

var ownCardBytes = []byte("own-peer-card-envelope")

func newWireRig(t *testing.T, maxContacts int) *wireRig {
	t.Helper()
	r := newRig(t, app.ModePublic)
	w := &wireRig{rig: r, chat: &fakeChat{}, clk: &tclock{t: testNow}}
	a, err := app.New(app.Config{
		Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: w.clk, Self: nid(1), Mode: app.ModePublic, Version: "0.3.0",
		MaxContacts: maxContacts,
		Chat:        w.chat,
		OwnCard:     func() ([]byte, bool) { return ownCardBytes, true },
		VerifyCard:  func(_ [32]byte, env []byte) bool { return string(env) == "card" },
		IngestCard:  func(from [32]byte, env []byte) { w.got = append(w.got, ingested{from, append([]byte(nil), env...)}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.a = a
	return w
}

// addContact adds the contact of nid(b) through a link, as the UI does.
func (w *wireRig) addContact(t *testing.T, b byte) string {
	t.Helper()
	url := "echofog://join?v=2&card=C" + string(rune('A'+b))
	w.ln.preview[url] = &links.Preview{Kind: links.KindContact, Node: nid(b), Device: nid(b + 100), NotAfter: testNow.Add(3600e9)}
	w.ln.peer = node.Peer{Node: node.NodeID(nid(b)), Device: node.DeviceID(nid(b + 100))}
	c, err := w.a.AddByLink(context.Background(), url)
	if err != nil {
		t.Fatalf("AddByLink %d: %v", b, err)
	}
	return c.Node
}

func knockAsk() *chatpb.Control {
	return &chatpb.Control{Kind: &chatpb.Control_Knock{Knock: &chatpb.Knock{Ask: true}}}
}

func TestContactLimitIsEnforcedBeforeDialing(t *testing.T) {
	w := newWireRig(t, 2)
	w.addContact(t, 7)
	w.addContact(t, 11)
	dialed := len(w.ln.redeemed)

	w.ln.preview["echofog://join?v=2&card=NEW"] = &links.Preview{Kind: links.KindContact, Node: nid(12), Device: nid(13), NotAfter: testNow.Add(3600e9)}
	if _, err := w.a.AddByLink(context.Background(), "echofog://join?v=2&card=NEW"); !errors.Is(err, app.ErrTooManyContacts) {
		t.Fatalf("third contact: %v, want ErrTooManyContacts", err)
	}
	if len(w.ln.redeemed) != dialed {
		t.Fatal("a link beyond the limit was dialed")
	}
	// an existing contact can be refreshed at the limit
	w.ln.peer = node.Peer{Node: node.NodeID(nid(7)), Device: node.DeviceID(nid(107))}
	if _, err := w.a.AddByLink(context.Background(), "echofog://join?v=2&card=C"+string(rune('A'+7))); err != nil {
		t.Fatalf("refreshing a contact at the limit: %v", err)
	}
	if got := app.UserMessage(app.ErrTooManyContacts); got == "Не удалось выполнить действие" || got == "" {
		t.Fatalf("no user text for the limit: %q", got)
	}
}

func TestFailedRedeemTextsAreUniform(t *testing.T) {
	w := newWireRig(t, 0)
	w.ln.preview["echofog://join?v=2&card=X"] = &links.Preview{Kind: links.KindContact, Node: nid(7), Device: nid(8), NotAfter: testNow.Add(3600e9)}
	var msgs []string
	for _, e := range []error{
		errors.New("peer says: you are in my contacts"),
		errors.New("peer says: you are not in my contacts"),
		errors.New("no session"),
	} {
		w.ln.redeemErr = e
		_, err := w.a.AddByLink(context.Background(), "echofog://join?v=2&card=X")
		if err == nil {
			t.Fatal("redeem error swallowed")
		}
		msgs = append(msgs, app.UserMessage(err))
	}
	if msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Fatalf("the user text depends on what the peer said: %q", msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m, "contacts") {
			t.Fatalf("peer text leaked into the user message: %q", m)
		}
	}
}

func TestKnockFromAContactIsAnsweredWithTheOwnCard(t *testing.T) {
	w := newWireRig(t, 0)
	w.addContact(t, 7)
	w.a.OnControl(nid(7), knockAsk())
	if w.chat.count() != 1 {
		t.Fatalf("sent %d messages, want the card answer", w.chat.count())
	}
	m := w.chat.sent[0]
	k := m.m.GetControl().GetKnock()
	if m.to != nid(7) || k == nil || k.GetAsk() || string(k.GetCard()) != string(ownCardBytes) {
		t.Fatalf("answer = to %x %+v", m.to[:2], k)
	}
}

func TestKnockFromStrangersAndBlockedContactsGetNoAnswer(t *testing.T) {
	w := newWireRig(t, 0)
	node7 := w.addContact(t, 7)
	w.a.OnControl(nid(50), knockAsk()) // nobody we know
	if err := w.a.Block(node7); err != nil {
		t.Fatal(err)
	}
	w.a.OnControl(nid(7), knockAsk())
	if w.chat.count() != 0 {
		t.Fatalf("%d answers: silence is the same for a stranger and a blocked contact", w.chat.count())
	}
}

func TestKnockAnswersAreThrottledPerContact(t *testing.T) {
	w := newWireRig(t, 0)
	w.addContact(t, 7)
	w.addContact(t, 11)
	for i := 0; i < 20; i++ {
		w.a.OnControl(nid(7), knockAsk())
	}
	if w.chat.count() != 1 {
		t.Fatalf("a flood of knocks got %d answers, want 1", w.chat.count())
	}
	w.a.OnControl(nid(11), knockAsk())
	if w.chat.count() != 2 {
		t.Fatal("another contact must still be answered")
	}
}

func TestCardAnswerReachesTheCardBookOnlyFromContactsAndBounded(t *testing.T) {
	w := newWireRig(t, 0)
	n7 := w.addContact(t, 7)
	if err := w.a.Knock(context.Background(), n7); err != nil { // only a card that was asked for is taken (T11.3d)
		t.Fatal(err)
	}
	asked := w.chat.count()
	answer := func(card []byte) *chatpb.Control {
		return &chatpb.Control{Kind: &chatpb.Control_Knock{Knock: &chatpb.Knock{Card: card}}}
	}
	w.a.OnControl(nid(50), answer([]byte("card"))) // a stranger
	w.a.OnControl(nid(7), answer(nil))             // empty
	w.a.OnControl(nid(7), answer(make([]byte, 1<<20)))
	if len(w.got) != 0 {
		t.Fatalf("ingested %d cards from strangers, empties or oversized answers", len(w.got))
	}
	w.a.OnControl(nid(7), answer([]byte("card")))
	if len(w.got) != 1 || w.got[0].from != nid(7) || string(w.got[0].env) != "card" {
		t.Fatalf("ingested %+v", w.got)
	}
	if w.chat.count() != asked {
		t.Fatal("an answer to a knock was answered again")
	}
}

func TestKnockAsksAContact(t *testing.T) {
	w := newWireRig(t, 0)
	n7 := w.addContact(t, 7)
	if err := w.a.Knock(context.Background(), n7); err != nil {
		t.Fatal(err)
	}
	if w.chat.count() != 1 || !w.chat.sent[0].m.GetControl().GetKnock().GetAsk() || w.chat.sent[0].to != nid(7) {
		t.Fatalf("sent %+v", w.chat.sent)
	}
	if err := w.a.Knock(context.Background(), hx(nid(60))); !errors.Is(err, app.ErrNoContact) {
		t.Fatalf("knock at a stranger: %v", err)
	}
	_ = w.a.Block(n7)
	if err := w.a.Knock(context.Background(), n7); !errors.Is(err, app.ErrBlocked) {
		t.Fatalf("knock at a blocked contact: %v", err)
	}
}

func TestControlGarbageIsIgnored(t *testing.T) {
	w := newWireRig(t, 0)
	w.addContact(t, 7)
	w.a.OnControl(nid(7), nil)
	w.a.OnControl(nid(7), &chatpb.Control{})
	w.a.OnControl(nid(7), &chatpb.Control{Kind: &chatpb.Control_Reset_{Reset_: &chatpb.Reset{}}})
	w.a.OnControl(nid(7), &chatpb.Control{Kind: &chatpb.Control_Knock{}})
	if w.chat.count() != 0 || len(w.got) != 0 {
		t.Fatal("garbage produced traffic")
	}
}

func TestKnockWithoutChatIsHarmless(t *testing.T) {
	r := newRig(t, app.ModePublic) // no Chat in the config
	if _, err := r.a.AddByLink(context.Background(), contactURL); err != nil {
		t.Fatal(err)
	}
	r.a.OnControl(nid(7), knockAsk())
	if err := r.a.Knock(context.Background(), hx(nid(7))); err == nil {
		t.Fatal("Knock without a chat service succeeded")
	}
}

func TestConnectedFollowsLiveSessions(t *testing.T) {
	r := newRig(t, app.ModePublic)
	if r.a.Connected(apitypes.PeerID(nid(7))) {
		t.Fatal("connected with no session")
	}
	r.rt.status = node.Status{Sessions: []node.SessionStatus{{Node: node.NodeID(nid(7)), Device: node.DeviceID(nid(8))}}}
	if !r.a.Connected(apitypes.PeerID(nid(7))) {
		t.Fatal("not connected despite a session")
	}
	if r.a.Connected(apitypes.PeerID(nid(9))) {
		t.Fatal("connected to a node without a session")
	}
}

func TestContactRecordsLiveUnderTheDocumentedPrefix(t *testing.T) {
	r := newRig(t, app.ModePublic)
	c, err := r.a.AddByLink(context.Background(), contactURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.kv.Get([]byte("c/" + c.Node)); err != nil {
		t.Fatalf("contact record is not under c/<hex node>: %v", err)
	}
	if err := r.a.DeleteContact(c.Node); err != nil {
		t.Fatal(err)
	}
	if _, err := r.kv.Get([]byte("c/" + c.Node)); err == nil {
		t.Fatal("DeleteContact left the record")
	}
}

func TestMoreContactsThanOneScanPageAreListedAndCounted(t *testing.T) {
	w := newWireRig(t, 1500)
	for i := 0; i < 1100; i++ { // more than store.MaxScanLimit
		rec := fmt.Sprintf(`{"d":"","a":"c%04d","t":1}`, i)
		if err := w.kv.Put([]byte(fmt.Sprintf("c/%064x", i+1000)), []byte(rec)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := w.a.Contacts()
	if err != nil || len(list) != 1100 {
		t.Fatalf("Contacts = %d, %v, want 1100", len(list), err)
	}
	w.addContact(t, 7) // below the limit of 1500 although the list is longer than a page
	small := newWireRig(t, 1050)
	for i := 0; i < 1100; i++ {
		_ = small.kv.Put([]byte(fmt.Sprintf("c/%064x", i+1000)), []byte(`{"a":"x","t":1}`))
	}
	small.ln.preview["echofog://join?v=2&card=Z"] = &links.Preview{Kind: links.KindContact, Node: nid(70), Device: nid(71), NotAfter: testNow.Add(3600e9)}
	if _, err := small.a.AddByLink(context.Background(), "echofog://join?v=2&card=Z"); !errors.Is(err, app.ErrTooManyContacts) {
		t.Fatalf("beyond the limit past one page: %v", err)
	}
}
