package dht

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// srvNow is the fixed corrected clock of the server tests: 2026-11-05 12:00 UTC, mid-day, so the
// day boundaries are far away.
var srvNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func seededSigner(t testing.TB, seed uint64) *keys.Signer {
	t.Helper()
	var s [32]byte
	binary.BigEndian.PutUint64(s[0:8], seed)
	binary.BigEndian.PutUint64(s[8:16], seed^0xa5a5a5a55a5a5a5a)
	sg, err := keys.NewEd25519Signer(s[:])
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

func signerID(sg *keys.Signer) [32]byte { return sg.Public().ID() }

// unknownRecord seals an object of an unknown echofog/ label (empty body).
func unknownRecord(t testing.TB, label string, sg *keys.Signer, seq uint64, notAfter time.Time) []byte {
	t.Helper()
	b, err := objects.Seal(label, sg, seq, uint64(notAfter.Unix()), nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hintRecord seals an empty mailbox hint, a known signer-keyed label.
func hintRecord(t testing.TB, sg *keys.Signer, seq uint64, notAfter time.Time) []byte {
	t.Helper()
	body, err := objects.Marshal(&pb.MailboxHint{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := objects.Seal(objects.LabelMailboxHint, sg, seq, uint64(notAfter.Unix()), body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dayKey(label string, sg *keys.Signer, now time.Time) [32]byte {
	return objects.DHTKey(label, signerID(sg), objects.DHTDay(now))
}

// newServer builds a server over a fixed clock.
func newServer(t testing.TB, mutate func(*ServerConfig)) *Server {
	t.Helper()
	cfg := ServerConfig{
		Self: DeviceID{0x77},
		Now:  func() time.Time { return srvNow },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var peerSeq uint64

// testPeer is a publishing session of the given diversity unit.
func testPeer(unit string) Peer {
	peerSeq++
	var sid [16]byte
	binary.BigEndian.PutUint64(sid[:], peerSeq)
	p := Peer{SID: sid}
	if unit != "" {
		p.Unit = netip.MustParsePrefix(unit)
	}
	binary.BigEndian.PutUint64(p.Device[:], peerSeq)
	return p
}

// keySet is the set of keys a test server is responsible for. Adding the key of the current day
// only keeps each STORE to one stored copy.
type keySet struct{ m map[[32]byte]bool }

func newKeySet() *keySet { return &keySet{m: map[[32]byte]bool{}} }

func (k *keySet) add(key [32]byte) [32]byte { k.m[key] = true; return key }

func (k *keySet) config(c *ServerConfig) { c.Responsible = func(key [32]byte) bool { return k.m[key] } }

// respondAll makes the server responsible for every key; each STORE then lands under the keys of
// all three accepted days.
func respondAll(c *ServerConfig) { c.Responsible = func([32]byte) bool { return true } }

// publicCard builds a card of a device seed with a public address.
func publicCard(t *testing.T, seed, seq uint64, scope objects.Scope, flags uint32) *objects.Card {
	t.Helper()
	// A distinct address and network per seed: the routing table keeps one entry per endpoint and
	// two per network in a bucket.
	host := fmt.Sprintf("%d.%d.%d.7", 20+(seed>>16)&0x7f, (seed>>8)&0xff, seed&0xff)
	return buildCard(t, seed, seq, srvNow, scope, flags, testAddr(host, 4430))
}

// checkInvariants fails the test when a counter, heap or cache disagrees with the stored records.
func (s *Server) checkInvariants(t testing.TB) {
	t.Helper()
	if err := s.checkInvariantsErr(); err != nil {
		t.Fatalf("server invariants: %v", err)
	}
}
