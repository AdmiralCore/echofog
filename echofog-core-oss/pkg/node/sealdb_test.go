package node

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/dto"
)

// A database written by an older version (everything in the open) is converted on the first start:
// the data is still there and readable, nothing personal is left in the open.
func TestOldDatabaseIsSealedOnStart(t *testing.T) {
	db := testutil.NewTestingBadgerStorage(t)
	defer db.CloseAndClear()
	raw := db.St

	me := "aa" + hex.EncodeToString(make([]byte, 31))
	peer := "bb" + hex.EncodeToString(make([]byte, 31))

	// old contact, alias, message, outbox item
	oldContacts := contacts.NewContactService(raw, nil)
	if err := oldContacts.NewContact(dto.ContactDTO{PeerID: peer, Alias: "Bob Secret"}); err != nil {
		t.Fatal(err)
	}
	_ = oldContacts.SetPeerAlias(peer, "Bobby")
	chat := messenger.GenerateLocalChatID(me, peer, false)
	oldKeeper := messenger.NewMessageKeeper(raw)
	for _, text := range []string{"first secret", "second secret"} {
		if err := oldKeeper.Setter.SaveMessage(0, chat, &messenger.PersonalMessageProto{Id: text, Text: text, SenderId: peer}); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Set([]byte("outbox/"+peer+"/00000000000000000001-x"), []byte(`{"id":"x","intent":"c2VjcmV0"}`))

	k := make([]byte, 32)
	_, _ = rand.Read(k)
	s, err := storage.NewSealed(raw, memguard.NewEnclave(k))
	if err != nil {
		t.Fatal(err)
	}
	if n := migrateToSealed(s); n != 5 {
		t.Fatalf("converted %d records, want 5", n)
	}
	if n := migrateToSealed(s); n != 0 {
		t.Fatalf("a second run converted %d records", n)
	}

	// nothing readable is left underneath: no texts, no names, no peer IDs in keys
	all, _ := raw.FindValues(nil, 0)
	for _, e := range all {
		for _, secret := range [][]byte{[]byte("secret"), []byte("Bob"), []byte(peer)} {
			if bytes.Contains(e.Key, secret) || bytes.Contains(e.Value, secret) {
				t.Fatalf("%q still shows %q", e.Key, secret)
			}
		}
	}

	// and through the services everything is there
	c := contacts.NewContactService(s, nil)
	if !c.Exists(peer) {
		t.Fatal("the contact is gone")
	}
	if list, _ := c.GiveMyContacts(); len(list) != 1 || list[0].Alias != "Bob Secret" {
		t.Fatalf("contacts: %+v", list)
	}
	if a, _ := c.GetPeerAlias(peer); a != "Bobby" {
		t.Fatalf("alias %q", a)
	}
	keeper := messenger.NewMessageKeeper(s)
	msgs, err := keeper.Getter.GetMessages(0, chat, 10, 0, me, peer)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("got %d messages (%v)", len(msgs), err)
	}
	// numbering goes on where it was
	if err := keeper.Setter.SaveMessage(0, chat, &messenger.PersonalMessageProto{Id: "3", Text: "third", SenderId: peer}); err != nil {
		t.Fatal(err)
	}
	if v, err := raw.Get(binary.BigEndian.AppendUint64([]byte("seq:msg:"), s.BlindChat(chat))); err != nil || binary.BigEndian.Uint64(v) != 3 {
		t.Fatalf("counter %v %v", v, err)
	}
	if msgs, _ = keeper.Getter.GetMessages(0, chat, 10, 0, me, peer); len(msgs) != 3 {
		t.Fatalf("got %d messages after a new one", len(msgs))
	}
}

// DHT records kept under their key in the open (it names a peer) are dropped once; records under
// the hashed key stay, and the new key shows nothing of the peer.
func TestOpenDHTRecordKeysAreDropped(t *testing.T) {
	db := testutil.NewTestingBadgerStorage(t)
	defer db.CloseAndClear()
	raw := db.St

	peer := "cc" + hex.EncodeToString(make([]byte, 31))
	open := append([]byte(dht.RecordPrefix), "bundle:"+peer...)
	hashed := dht.RecordKey([]byte("bundle:" + peer))
	if bytes.Contains(hashed, []byte(peer)) {
		t.Fatalf("the record key %q still names the peer", hashed)
	}
	for _, k := range [][]byte{open, hashed} {
		if err := raw.Set(k, []byte("rec")); err != nil {
			t.Fatal(err)
		}
	}

	dropOpenDHTRecords(raw)
	if raw.Exists(open) {
		t.Fatal("the record under the open key is still there")
	}
	if !raw.Exists(hashed) {
		t.Fatal("the record under the hashed key was dropped")
	}

	// only once: a later open key (which no current code writes) is left alone
	_ = raw.Set(open, []byte("rec"))
	dropOpenDHTRecords(raw)
	if !raw.Exists(open) {
		t.Fatal("the cleanup ran a second time")
	}
}
