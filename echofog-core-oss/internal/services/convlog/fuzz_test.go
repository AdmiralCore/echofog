package convlog

import (
	"crypto/ed25519"
	"sync"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// FuzzDecodeEvent: events arrive from any member (push, sync answers). Decoding must not panic and
// must accept only what re-encodes to an event with the same id.
func FuzzDecodeEvent(f *testing.F) {
	fz := fuzzWorld()
	f.Add(fz.event(fz.founder, 1, 1, KindMsg, nil, mustMarshal(&MsgBody{Text: "hi"})))
	f.Add([]byte{})
	f.Add([]byte{0x0a, 0x10})
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, id, err := Decode(raw)
		if err != nil {
			return
		}
		again, err := Encode(e)
		if err != nil {
			return // a non-canonical encoding may grow past the cap
		}
		e2, id2, err := Decode(again)
		if err != nil || id2 != id || AuthorID(e2) != AuthorID(e) {
			t.Fatalf("re-encoding a decoded event changed it: %v", err)
		}
	})
}

// FuzzDecodeSync: sync messages (hello, want, events, ihave) are parsed before anything checks them.
func FuzzDecodeSync(f *testing.F) {
	raw, _ := EncodeSync(&Sync{M: &Sync_Want{Want: &Want{Conv: make([]byte, convIDLen), Have: []*Have{{Author: make([]byte, 32), Seq: 3}}}}})
	f.Add(raw)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = DecodeSync(raw)
	})
}

// FuzzIngestSigned signs events with fields chosen by the fuzzer, by the group's founder or by a
// stranger, and feeds them to a log as a push from the network would: whatever the kind, body,
// references or counters, applying them and reading the conversation back must not panic. The
// log is shared by the iterations of one process, so it builds up state as a real one does (a
// crash found this way may need the inputs before it to repeat).
func FuzzIngestSigned(f *testing.F) {
	fz := fuzzWorld()
	member, _ := proto.Marshal(&MemberBody{Member: fz.stranger.pub, Role: "member"})
	f.Add(true, uint64(1), uint64(1), KindMember, uint8(0), member)
	f.Add(true, uint64(2), uint64(2), KindMsg, uint8(0), mustMarshal(&MsgBody{Text: "hello"}))
	f.Add(false, uint64(1), uint64(3), KindMsg, uint8(0), mustMarshal(&MsgBody{Text: "me too"}))
	f.Add(true, uint64(3), uint64(4), KindEdit, uint8(1), mustMarshal(&EditBody{Text: "edited"}))
	f.Add(false, uint64(2), uint64(5), KindReact, uint8(1), mustMarshal(&ReactBody{Emoji: "👍", On: true}))
	f.Add(true, uint64(4), uint64(6), KindDelete, uint8(1), []byte(nil))
	f.Add(true, uint64(5), uint64(7), KindChannel, uint8(0), []byte{0x0a, 0x01, 'x'})
	f.Add(true, uint64(6), uint64(8), KindThread, uint8(1), []byte{0x0a, 0x01, 't'})
	f.Add(true, uint64(7), uint64(9), KindProfile, uint8(0), []byte{0x0a, 0x03, 1, 2, 3})
	f.Add(true, uint64(1<<62), uint64(1<<63), uint32(0xffffffff), uint8(7), []byte{0xff})
	f.Add(true, uint64(8), uint64(10), KindMsg, uint8(0), mustMarshal(&MsgBody{Text: "файл", Att: goodAtt()}))
	f.Add(false, uint64(4), uint64(11), KindMsg, uint8(0), mustMarshal(&MsgBody{Text: "чужой", Att: goodAtt()}))
	f.Add(true, uint64(9), uint64(12), KindClose, uint8(0), []byte(nil))
	f.Add(false, uint64(5), uint64(13), KindClose, uint8(0), mustMarshal(&CloseBody{Last: []*SeqEntry{{Author: make([]byte, 32), Seq: 1}}}))
	f.Fuzz(func(t *testing.T, byFounder bool, seq, lamport uint64, kind uint32, ref uint8, body []byte) {
		fz.mu.Lock()
		defer fz.mu.Unlock()
		author := fz.stranger
		if byFounder {
			author = fz.founder
		}
		var target []byte
		if len(fz.ids) > 0 && ref > 0 {
			id := fz.ids[int(ref)%len(fz.ids)]
			target = id[:]
		}
		wasClosed := fz.log.Closed(fz.group)
		for _, c := range []Conv{fz.group, DM(fz.self, author.id)} {
			raw := fz.eventIn(c, author, seq, lamport, kind, target, body)
			if id, ok, _ := fz.log.Ingest(c, raw); ok && len(fz.ids) < 64 {
				fz.ids = append(fz.ids, id)
			}
			_, _, _ = fz.log.History(c, "", 20)
			_ = fz.log.Channels(c)
			_ = fz.log.Roles(c)
			_ = fz.log.RightsOf(c, author.id)
			_, _ = fz.log.Meta(c)
		}
		// a group is closed only by its founder
		if !wasClosed && !byFounder && fz.log.Closed(fz.group) {
			t.Fatal("a group was closed by somebody else")
		}
	})
}

type fuzzState struct {
	mu                sync.Mutex
	log               *Log
	self              types.PeerID
	founder, stranger fuzzAccount
	group             Conv
	ids               []EventID
}

type fuzzAccount struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	id   types.PeerID
}

var (
	fuzzOnce sync.Once
	fuzzS    *fuzzState
)

func fuzzWorld() *fuzzState {
	fuzzOnce.Do(func() {
		acc := func(seed byte) fuzzAccount {
			s := make([]byte, ed25519.SeedSize)
			s[0] = seed
			priv := ed25519.NewKeyFromSeed(s)
			pub := priv.Public().(ed25519.PublicKey)
			var pk types.PeerPublicKey
			copy(pk[:], pub)
			return fuzzAccount{pub: pub, priv: priv, id: types.PeerPubKeyToID(pk)}
		}
		self, founder, stranger := acc(1), acc(2), acc(3)
		db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil).WithMemTableSize(8 << 20))
		if err != nil {
			panic(err)
		}
		l := New(storage.NewBadgerStorage(db), self.pub, func(b []byte) ([]byte, error) { return ed25519.Sign(self.priv, b), nil })
		l.SetGroupRules(testRules{founder: founder.id})
		fuzzS = &fuzzState{log: l, self: self.id, founder: founder, stranger: stranger, group: Group("fuzz-group")}
	})
	return fuzzS
}

func (fz *fuzzState) event(a fuzzAccount, seq, lamport uint64, kind uint32, target, body []byte) []byte {
	return fz.eventIn(fz.group, a, seq, lamport, kind, target, body)
}

func (fz *fuzzState) eventIn(c Conv, a fuzzAccount, seq, lamport uint64, kind uint32, target, body []byte) []byte {
	e := &Event{Conv: c.ID[:], Author: a.pub, Seq: seq, Lamport: lamport, Kind: kind, Target: target, Body: body}
	if err := Sign(e, func(b []byte) ([]byte, error) { return ed25519.Sign(a.priv, b), nil }); err != nil {
		panic(err)
	}
	raw, err := Encode(e)
	if err != nil {
		return nil
	}
	return raw
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
