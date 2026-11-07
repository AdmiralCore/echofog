package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/convsync"
)

// ---- T11.3d: what arrives through chat/2 ---------------------------------------------------------------------

func dmRef(n *gNode) app.ChatRef { return app.ChatRef{Kind: "dm", ID: n.nodeHex()} }

func historyLen(t *testing.T, n *gNode, ref app.ChatRef) int {
	t.Helper()
	p, err := n.app.ChatHistory(context.Background(), ref, "", 50)
	if err != nil {
		t.Fatalf("ChatHistory: %v", err)
	}
	return len(p.Messages)
}

// A blocked contact's direct messages never reach the log; the same message from a contact that is not
// blocked does.
func TestBlockedContactsDirectMessagesAreDropped(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	alice.befriend(t, bob)
	bob.befriend(t, alice)
	ctx := context.Background()
	if _, err := bob.app.ChatSend(ctx, dmRef(alice), app.SendReq{Text: "до блокировки"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if got := historyLen(t, alice, dmRef(bob)); got != 1 {
		t.Fatalf("before the block alice has %d messages, want 1", got)
	}
	if err := alice.app.Block(bob.nodeHex()); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.app.ChatSend(ctx, dmRef(alice), app.SendReq{Text: "после блокировки"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if got := historyLen(t, alice, dmRef(bob)); got != 1 {
		t.Fatalf("after the block alice has %d messages, want still 1", got)
	}
	if err := alice.app.Unblock(bob.nodeHex()); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.app.ChatSend(ctx, dmRef(alice), app.SendReq{Text: "после разблокировки"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if got := historyLen(t, alice, dmRef(bob)); got < 2 {
		t.Fatalf("after the unblock alice has %d messages, want the new one in", got)
	}
}

// The log of a group is shared: its events are taken from a blocked member as from any other, so that
// the logs do not drift apart.
func TestBlockedMembersGroupEventsStillReachTheLog(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	alice.befriend(t, bob)
	if err := alice.app.Block(bob.nodeHex()); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.app.ChatSend(context.Background(), gref(id, ""), app.SendReq{Text: "в группе"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	if got := historyLen(t, alice, gref(id, "")); got != 1 {
		t.Fatalf("alice sees %d group messages, want 1", got)
	}
}

func TestBlockedContactsFilesAreDropped(t *testing.T) {
	a, b := pair(t, app.FileLimits{})
	m, _ := offerMessage(t, a.node, bytes.Repeat([]byte{3}, 400), "x.bin")
	m.Type = chatpb.PersonalMessageType_MSG_TYPE_FILE
	ctx := context.Background()
	if !b.app.OnIncoming(ctx, a.node, m) {
		t.Fatal("a file offer was not taken as one")
	}
	if l, _ := b.app.ListFileTransfers(""); len(l) != 1 || l[0].State != app.FileOffered {
		t.Fatalf("a contact's offer: %+v", l)
	}
	if err := b.app.Block(a.hex()); err != nil {
		t.Fatal(err)
	}
	m2, _ := offerMessage(t, a.node, bytes.Repeat([]byte{4}, 400), "y.bin")
	if !b.app.OnIncoming(ctx, a.node, m2) {
		t.Fatal("a blocked contact's offer was passed on as chat")
	}
	if l, _ := b.app.ListFileTransfers(""); len(l) != 1 {
		t.Fatalf("a blocked contact's offer was listed: %+v", l)
	}
}

// A group member who is no contact may offer a file: the group is named by the receiver from its own
// roster, never by the sender (L2 of the T11.3c acceptance).
func TestGroupFileOfferFromAMemberIsTakenInThatGroup(t *testing.T) {
	w, ns, id := team(t, 3)
	alice, bob, carol := ns[0], ns[1], ns[2]
	_ = w
	m, _ := offerMessage(t, bob.node, bytes.Repeat([]byte{5}, 300), "g.bin")
	ctx := context.Background()
	if !alice.app.OnIncoming(ctx, bob.node, m) {
		t.Fatal("the offer was not taken as one")
	}
	l, _ := alice.app.ListFileTransfers("")
	if len(l) != 1 || l[0].Group != id {
		t.Fatalf("a member's offer: %+v, want it in group %s", l, id)
	}
	// someone who shares no group with alice and is no contact is refused, silently
	stranger := newStranger(t)
	m2, _ := offerMessage(t, stranger, bytes.Repeat([]byte{6}, 300), "s.bin")
	if !alice.app.OnIncoming(ctx, stranger, m2) {
		t.Fatal("a stranger's offer was passed on as chat")
	}
	if l, _ := alice.app.ListFileTransfers(""); len(l) != 1 {
		t.Fatalf("a stranger's offer was listed: %+v", l)
	}
	_ = carol
}

func newStranger(t *testing.T) [32]byte {
	t.Helper()
	var n [32]byte
	for i := range n {
		n[i] = 0xEE
	}
	return n
}

func TestOnIncomingTakesJoinMessagesAndChatEvents(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	g, _ := alice.app.CreateGroup(ctx, "Г", "", app.GroupPrivate)
	inv, _ := alice.app.GroupInviteCreate(ctx, g.ID, app.InviteSpec{TTL: time.Hour})
	if _, err := bob.app.JoinGroup(ctx, inv.Link); err != nil {
		t.Fatal(err)
	}
	w.drain() // the join message goes through OnIncoming now
	if got := len(memberRoles(t, alice, g.ID)); got != 2 {
		t.Fatalf("members after the join: %d", got)
	}
	if bob.app.OnIncoming(ctx, alice.node, &chatpb.PersonalMessageProto{Text: "обычный текст без журнала"}) {
		t.Fatal("a plain message with no log event was taken")
	}
}

// ChatEvents is what chat.ServiceConfig.Events becomes: messages to the router, statuses to the
// tracker, controls to the knock handling.
func TestChatEventsFeedTheApp(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	alice.befriend(t, bob)
	bob.befriend(t, alice)
	st := app.NewChatStates()
	ev := alice.app.ChatEvents(st)
	if ev.Message == nil || ev.Status == nil || ev.Control == nil {
		t.Fatalf("events %+v", ev)
	}
	ctx := context.Background()
	if _, err := bob.app.ChatSend(ctx, dmRef(alice), app.SendReq{Text: "через события"}); err != nil {
		t.Fatal(err)
	}
	// carry the letter as the chat service would: through Events.Message
	w.mu.Lock()
	var pm *chatpb.PersonalMessageProto
	for _, l := range w.queue {
		if l.to == alice.node && len(l.pm.GetConvEvent()) > 0 {
			pm = l.pm
		}
	}
	w.queue = nil
	w.mu.Unlock()
	if pm == nil {
		t.Fatal("no letter for alice")
	}
	ev.Message(chat.Incoming{ID: [16]byte{9}, From: bob.node, Message: pm})
	if got := historyLen(t, alice, dmRef(bob)); got != 1 {
		t.Fatalf("alice has %d messages after Events.Message, want 1", got)
	}
	// a status of a message alice sent moves the tracker
	sent, err := alice.app.ChatSend(ctx, dmRef(bob), app.SendReq{Text: "ответ"})
	if err != nil {
		t.Fatal(err)
	}
	ev.Status([16]byte{}, chat.StateDelivered) // unknown id: nothing happens, nothing panics
	if s, _ := st.StateOf(sent.ID); s == app.StateDelivered {
		t.Fatalf("an unknown status id marked a message delivered")
	}
}

// ---- the card a contact answers a knock with ---------------------------------------------------------------

func knockAnswer(card []byte) *chatpb.Control {
	return &chatpb.Control{Kind: &chatpb.Control_Knock{Knock: &chatpb.Knock{Card: card}}}
}

func TestCardAnswerWithoutAKnockIsIgnored(t *testing.T) {
	w := newWireRig(t, 0)
	w.addContact(t, 7)
	w.a.OnControl(nid(7), knockAnswer([]byte("card")))
	if len(w.got) != 0 {
		t.Fatalf("an unasked card was ingested: %+v", w.got)
	}
}

func TestCardAnswerAfterAKnockIsIngestedOnceAndOnlyIfValid(t *testing.T) {
	w := newWireRig(t, 0)
	n7 := w.addContact(t, 7)
	ctx := context.Background()
	if err := w.a.Knock(ctx, n7); err != nil {
		t.Fatal(err)
	}
	w.a.OnControl(nid(7), knockAnswer([]byte("forged"))) // the verifier refuses it
	if len(w.got) != 0 {
		t.Fatalf("a card that does not verify was ingested: %+v", w.got)
	}
	w.a.OnControl(nid(7), knockAnswer([]byte("card")))
	if len(w.got) != 1 || string(w.got[0].env) != "card" {
		t.Fatalf("the asked card: %+v", w.got)
	}
	w.a.OnControl(nid(7), knockAnswer([]byte("card"))) // the knock was answered; a second card is unasked
	if len(w.got) != 1 {
		t.Fatalf("one knock took two cards: %+v", w.got)
	}
}

func TestAKnockWaitsOnlyAShortTime(t *testing.T) {
	w := newWireRig(t, 0)
	n7 := w.addContact(t, 7)
	if err := w.a.Knock(context.Background(), n7); err != nil {
		t.Fatal(err)
	}
	w.clk.Advance(10 * time.Minute)
	w.a.OnControl(nid(7), knockAnswer([]byte("card")))
	if len(w.got) != 0 {
		t.Fatalf("a card long after the knock was ingested: %+v", w.got)
	}
}

// Without a verifier of its own the app checks the card for real: junk is no card.
func TestDefaultCardCheckRefusesJunkAndAnotherAccount(t *testing.T) {
	r := newRig(t, app.ModePublic)
	ch := &fakeChat{}
	var got [][]byte
	a, err := app.New(app.Config{
		Runtime: r.rt, Links: r.ln, Store: r.kv, Clock: clock{}, Self: nid(1), Mode: app.ModePublic, Version: "0.3.0",
		Chat:       ch,
		IngestCard: func(_ [32]byte, env []byte) { got = append(got, env) },
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &wireRig{rig: r, chat: ch}
	w.a = a
	n7 := w.addContact(t, 7)
	if err := a.Knock(context.Background(), n7); err != nil {
		t.Fatal(err)
	}
	a.OnControl(nid(7), knockAnswer([]byte("not a card at all")))
	if len(got) != 0 {
		t.Fatalf("junk was ingested: %q", got)
	}
}

// ---- T7.7: mentions, notification modes, followed threads, the voice roster ----------------------------------

func TestMentionsMarkTheMessageAndCountApart(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	if _, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "привет, @боб", Mentions: []string{bob.nodeHex()}}); err != nil {
		t.Fatalf("send with a mention: %v", err)
	}
	if _, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "без упоминаний"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	page, err := bob.app.ChatHistory(ctx, gref(id, ""), "", 10)
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("history %+v, %v", page, err)
	}
	var named, plain int
	for _, m := range page.Messages {
		if m.Mentioned {
			named++
		} else {
			plain++
		}
	}
	if named != 1 || plain != 1 {
		t.Fatalf("mentioned %d, plain %d, want 1 and 1", named, plain)
	}
	chats, _ := bob.app.Conversations()
	var mentions int
	for _, c := range chats {
		if c.Ref == gref(id, "") {
			mentions = c.Mentions
		}
	}
	if mentions != 1 {
		t.Fatalf("the chat list counts %d mentions, want 1", mentions)
	}
}

func TestMentionRefusals(t *testing.T) {
	_, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	if _, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "x", Mentions: []string{"zz"}}); !errors.Is(err, app.ErrBadMessage) {
		t.Errorf("a bad id: %v", err)
	}
	many := make([]string, convlog.MaxMentions+1)
	for i := range many {
		many[i] = bob.nodeHex()
	}
	if _, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "x", Mentions: many}); err == nil {
		t.Error("more mentions than the log takes were sent")
	}
	// a plain member may not name everybody
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "@all", MentionAll: true}); err == nil {
		t.Error("a member named everybody")
	}
	if _, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "@all", MentionAll: true}); err != nil {
		t.Errorf("the owner names everybody: %v", err)
	}
}

func TestNotificationModeIsLocalAndPerChannel(t *testing.T) {
	_, ns, id := team(t, 2)
	bob := ns[1]
	ref := gref(id, "")
	if m, err := bob.app.ChatNotify(ref); err != nil || m != app.NotifyAll {
		t.Fatalf("default mode %q, %v", m, err)
	}
	if err := bob.app.SetChatNotify(ref, app.NotifyMentions); err != nil {
		t.Fatal(err)
	}
	if m, _ := bob.app.ChatNotify(ref); m != app.NotifyMentions {
		t.Fatalf("mode %q after setting it", m)
	}
	if m, _ := ns[0].app.ChatNotify(ref); m != app.NotifyAll {
		t.Fatalf("another device's mode moved: %q", m)
	}
	if err := bob.app.SetChatNotify(ref, "loud"); err == nil {
		t.Fatal("an unknown mode was taken")
	}
	if err := bob.app.SetChatNotify(app.ChatRef{Kind: "group", ID: "nope"}, app.NotifyNone); !errors.Is(err, app.ErrNoChat) {
		t.Fatalf("a chat that is not there: %v", err)
	}
}

func TestFollowedThreadsAreKeptPerChat(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	root, err := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "корень"})
	if err != nil {
		t.Fatal(err)
	}
	w.drain()
	if err := bob.app.ChatFollow(gref(id, ""), root.ID, true); err != nil {
		t.Fatal(err)
	}
	list, err := bob.app.ChatFollowing(gref(id, ""))
	if err != nil || len(list) != 1 || list[0] != root.ID {
		t.Fatalf("following %v, %v", list, err)
	}
	if err := bob.app.ChatFollow(gref(id, ""), root.ID, false); err != nil {
		t.Fatal(err)
	}
	if list, _ := bob.app.ChatFollowing(gref(id, "")); len(list) != 0 {
		t.Fatalf("still following %v", list)
	}
	if err := bob.app.ChatFollow(gref(id, ""), "xyz", true); !errors.Is(err, app.ErrBadMessage) {
		t.Fatalf("a bad thread id: %v", err)
	}
}

type fakeLive struct{ send int }

func (f *fakeLive) SendLive(context.Context, [32]byte, []byte) error { f.send++; return nil }

func TestVoiceRoomsRosterComesFromTheLiveRoster(t *testing.T) {
	_, ns, id := team(t, 2)
	alice := ns[0]
	ctx := context.Background()
	ch, err := alice.app.GroupChannelSave(ctx, id, convlog.ChannelSpec{ID: "room", Name: "Голос", Voice: true})
	if err != nil {
		t.Fatalf("voice channel: %v", err)
	}
	vr := convsync.NewVoiceRoster(convsync.VoiceConfig{
		Pub: alice.pub, Log: alice.eng.Log(), Dir: alice.world, Net: &fakeLive{},
		Groups: alice.book.Conv, Members: alice.book.Members, Now: func() time.Time { return testNow },
	})
	rooms := app.NewVoiceRooms(vr, alice.book)
	if got := rooms.Roster(id, ch.ID); len(got) != 0 {
		t.Fatalf("an empty room lists %v", got)
	}
	if err := vr.Join(ctx, convlog.Group(id), ch.ID, convsync.VoiceState{Mic: true}); err != nil {
		t.Fatalf("join: %v", err)
	}
	got := rooms.Roster(id, ch.ID)
	if len(got) != 1 || got[0].Node != alice.nodeHex() || got[0].Muted {
		t.Fatalf("roster %+v, want alice with the mic on", got)
	}
	if got := rooms.Roster("no-such-group", ch.ID); len(got) != 0 {
		t.Fatalf("an unknown group lists %v", got)
	}
}

type idSender struct{ id [16]byte }

func (s idSender) Send(context.Context, [32]byte, *chatpb.PersonalMessageProto) ([16]byte, error) {
	return s.id, nil
}

// chat/2 never takes a message back from a mailbox to the waiting state; a status that says so is
// late or wrong and must not turn "in a mailbox" into "waits for a connection".
func TestDeliveryStatusNeverGoesBackwards(t *testing.T) {
	w, ns := newGroupWorld(t, 2)
	alice, bob := ns[0], ns[1]
	alice.befriend(t, bob)
	bob.befriend(t, alice)
	if _, err := alice.app.ChatSend(context.Background(), dmRef(bob), app.SendReq{Text: "статус"}); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	var pm *chatpb.PersonalMessageProto
	for _, l := range w.queue {
		if len(l.pm.GetConvEvent()) > 0 {
			pm = l.pm
		}
	}
	w.mu.Unlock()
	if pm == nil {
		t.Fatal("no event was sent")
	}
	_, eid, err := convlog.Decode(pm.GetConvEvent())
	if err != nil {
		t.Fatal(err)
	}
	st := app.NewChatStates()
	id := [16]byte{7}
	if err := st.Wrap(idSender{id}).Send(context.Background(), bob.node, pm); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.StateOf(eid.String()); s != app.StateOffline {
		t.Fatalf("a fresh message is %q, want offline", s)
	}
	st.OnStatus(id, chat.StateMailbox)
	if s, _ := st.StateOf(eid.String()); s != app.StateRelayed {
		t.Fatalf("after the mailbox: %q", s)
	}
	st.OnStatus(id, chat.StateWaiting)
	if s, _ := st.StateOf(eid.String()); s != app.StateRelayed {
		t.Fatalf("a late waiting status turned it into %q", s)
	}
	st.OnStatus(id, chat.StateDelivered)
	if s, _ := st.StateOf(eid.String()); s != app.StateDelivered {
		t.Fatalf("after the confirmation: %q", s)
	}
}

// A message names its author by the v1 peer id; the contacts and the members of a group are known by
// the node id. NodeOfPeer joins the two, for the author of a direct message and of a group alike.
func TestNodeOfPeerJoinsTheTwoIdNames(t *testing.T) {
	w, ns, id := team(t, 2)
	alice, bob := ns[0], ns[1]
	ctx := context.Background()
	if _, err := bob.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "я тут"}); err != nil {
		t.Fatal(err)
	}
	w.drain()
	page, err := alice.app.ChatHistory(ctx, gref(id, ""), "", 10)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("history %+v, %v", page, err)
	}
	author := page.Messages[0].Author
	if author == bob.nodeHex() {
		t.Fatal("the author is already a node id: the two names are one, nothing to join")
	}
	if n, ok := alice.app.NodeOfPeer(author); !ok || n != bob.nodeHex() {
		t.Fatalf("NodeOfPeer(author) = %q, %v, want bob's node id", n, ok)
	}
	// oneself
	own, _ := alice.app.ChatSend(ctx, gref(id, ""), app.SendReq{Text: "и я"})
	if n, ok := alice.app.NodeOfPeer(own.Author); !ok || n != alice.nodeHex() {
		t.Fatalf("NodeOfPeer(own) = %q, %v", n, ok)
	}
	for _, bad := range []string{"", "zz", "00", strings.Repeat("ab", 32)} {
		if n, ok := alice.app.NodeOfPeer(bad); ok {
			t.Errorf("NodeOfPeer(%q) = %q for nobody", bad, n)
		}
	}
}

// Every refusal of the chat, group and file calls reaches the screen as a Russian text in the neutral
// form, never as the internal sentence, and the refusals the sender must not tell apart read alike.
func TestUserMessageSpeaksOfChatsGroupsAndFiles(t *testing.T) {
	generic := app.UserMessage(errors.New("anything else"))
	errs := []error{app.ErrNoChat, app.ErrChatOff, app.ErrBadMessage, app.ErrEmptyMessage, app.ErrMessageTooLong, app.ErrBadMode,
		app.ErrGroupOff, app.ErrInvite, app.ErrNoGroup, app.ErrBadGroup, app.ErrNoRight, app.ErrOwnerStays, app.ErrNoMember, app.ErrStillIn,
		app.ErrNoVoice, app.ErrFilesOff, app.ErrNoTransfer, app.ErrBadFile, app.ErrFileTooLarge, app.ErrTooManyTransfers, app.ErrBadState,
		convlog.ErrForbidden, convlog.ErrNotParticipant}
	for _, err := range errs {
		msg := app.UserMessage(err)
		if msg == generic || msg == "" || strings.Contains(msg, "app:") || !hasCyrillic(msg) {
			t.Errorf("%v: the screen says %q", err, msg)
		}
	}
	// an unknown group, a chat that is not there and a chat one is not in are one answer
	if app.UserMessage(app.ErrNoChat) != app.UserMessage(app.ErrNoGroup) || app.UserMessage(app.ErrNoChat) != app.UserMessage(convlog.ErrNotParticipant) {
		t.Error("the refusals that must look alike read differently")
	}
	if app.UserMessage(fmt.Errorf("wrapped: %w", app.ErrNoRight)) != app.UserMessage(app.ErrNoRight) {
		t.Error("a wrapped error is not recognised")
	}
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 'А' && r <= 'я' {
			return true
		}
	}
	return false
}
