package node_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// settableClock is a raw clock a test moves forward.
type settableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settableClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newPublishNode(t *testing.T, raw *settableClock) *testNode {
	t.Helper()
	clk, err := clock.New(clock.Config{Now: raw.Now})
	if err != nil {
		t.Fatalf("clock.New: %v", err)
	}
	nd := newTestNodeWith(t, cards.Visible, true, nil, nil, func(c *node.Config) { c.Clock = clk })
	nd.start(t)
	return nd
}

// ownDevices returns the devices/2 record among the node's own DHT records, verified at now.
func ownDevices(t *testing.T, nd *testNode, now time.Time) (*objects.DeviceList, []byte) {
	t.Helper()
	var found *objects.DeviceList
	var raw []byte
	for _, rec := range node.OwnDHTRecords(nd.rt) {
		env, err := objects.Verify(rec, now)
		if err != nil {
			t.Fatalf("own record does not verify: %v", err)
		}
		if env.Label != objects.LabelDevices {
			continue
		}
		dl, err := objects.VerifyDeviceList(rec, now)
		if err != nil {
			t.Fatalf("own device list: %v", err)
		}
		if found != nil {
			t.Fatal("two device lists among the own records")
		}
		found, raw = dl, rec
	}
	return found, raw
}

// The node publishes its account's device list under the node ID next to the public card
// (objects.md §6, dht.md §2): this is the first step of the chain node ID → devices → card.
func TestOwnRecordsContainDeviceListAndPublicCard(t *testing.T) {
	raw := &settableClock{now: time.Now()}
	nd := newPublishNode(t, raw)

	dl, _ := ownDevices(t, nd, raw.Now())
	if dl == nil {
		t.Fatal("no devices/2 among the own DHT records")
	}
	if dl.NodeID != [32]byte(nd.id.NodeID) {
		t.Fatalf("device list is signed for node %x, want %x", dl.NodeID[:4], nd.id.NodeID[:4])
	}
	if !dl.Contains(nd.device.Public()) {
		t.Fatal("the list does not name this device")
	}
	var cardSeen bool
	for _, rec := range node.OwnDHTRecords(nd.rt) {
		if c, err := objects.VerifyCard(rec, raw.Now()); err == nil {
			cardSeen = true
			if !c.Publishable() {
				t.Fatalf("own record is a non-publishable card (scope %v)", c.Scope)
			}
		}
	}
	if !cardSeen {
		t.Fatal("no public card among the own records")
	}
}

// A list is issued again when less than 10 days remain (objects.md §6) with a higher seq, and
// the issued list is kept in the store (AD13).
func TestDeviceListIsReissuedWhenTenDaysRemain(t *testing.T) {
	raw := &settableClock{now: time.Now()}
	nd := newPublishNode(t, raw)

	first, firstRaw := ownDevices(t, nd, raw.Now())
	if first == nil {
		t.Fatal("no devices/2")
	}
	if left := time.Unix(int64(first.Env.NotAfter), 0).Sub(raw.Now()); left > 30*24*time.Hour {
		t.Fatalf("lifetime %v exceeds 30 days", left)
	}

	raw.add(19 * 24 * time.Hour) // 11 days remain: no new list
	same, sameRaw := ownDevices(t, nd, raw.Now())
	if same == nil || !bytes.Equal(sameRaw, firstRaw) {
		t.Fatal("the list was issued again with 11 days remaining")
	}

	raw.add(2 * 24 * time.Hour) // 9 days remain
	next, _ := ownDevices(t, nd, raw.Now())
	if next == nil {
		t.Fatal("no devices/2 after 21 days")
	}
	if next.Env.Seq <= first.Env.Seq {
		t.Fatalf("seq %d after %d: the reader memory would refuse it", next.Env.Seq, first.Env.Seq)
	}
	if next.Env.NotAfter <= first.Env.NotAfter {
		t.Fatal("the new list does not live longer")
	}
	if left := time.Unix(int64(next.Env.NotAfter), 0).Sub(raw.Now()); left < 25*24*time.Hour {
		t.Fatalf("new list has only %v left", left)
	}

	kept, err := nd.store.Store(node.PublishNamespace).Get([]byte("devices"))
	if err != nil || len(kept) == 0 {
		t.Fatalf("the issued list is not kept in the store: %v", err)
	}
}

// A seq that restarts from a lost store must not be below what readers saw: it follows the clock.
func TestDeviceListSeqFollowsTheClock(t *testing.T) {
	raw := &settableClock{now: time.Now()}
	nd := newPublishNode(t, raw)
	dl, _ := ownDevices(t, nd, raw.Now())
	if dl == nil {
		t.Fatal("no devices/2")
	}
	if dl.Env.Seq < uint64(raw.Now().Unix()) {
		t.Fatalf("seq %d is below the clock %d", dl.Env.Seq, raw.Now().Unix())
	}
}

func sealOwnHint(t *testing.T, nd *testNode, seq uint64, notAfter time.Time) []byte {
	t.Helper()
	body, err := objects.Marshal(&pb.MailboxHint{})
	if err != nil {
		t.Fatal(err)
	}
	var rec []byte
	err = nd.device.WithSigner(func(s *keys.Signer) error {
		var err error
		rec, err = objects.Seal(objects.LabelMailboxHint, s, seq, uint64(notAfter.Unix()), body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// Prekeys and mailbox hints go through the same helper (T7.1, T7.4): an own record is accepted.
func TestPublishOwnAcceptsOwnRecords(t *testing.T) {
	raw := &settableClock{now: time.Now()}
	nd := newPublishNode(t, raw)

	if err := nd.rt.PublishOwn(sealOwnHint(t, nd, 1, raw.Now().Add(time.Hour))); err != nil {
		t.Fatalf("own mailbox hint: %v", err)
	}
	pub, ok := nd.rt.Cards().Own(objects.ScopePublic)
	if !ok {
		t.Fatal("no public card")
	}
	if err := nd.rt.PublishOwn(pub); err != nil {
		t.Fatalf("own public card: %v", err)
	}
	_, devRaw := ownDevices(t, nd, raw.Now())
	if err := nd.rt.PublishOwn(devRaw); err != nil {
		t.Fatalf("own device list: %v", err)
	}
}

// The helper never lets a record out that is not this node's own, nor a card that is not public
// (objects.md §3.3): a peer card carries the noise3 masking keys.
func TestPublishOwnRefusesForeignAndNonPublic(t *testing.T) {
	raw := &settableClock{now: time.Now()}
	nd := newPublishNode(t, raw)
	other := newPublishNode(t, raw)

	peer, ok := nd.rt.Cards().Own(objects.ScopePeer)
	if !ok {
		t.Fatal("no peer card")
	}
	if err := nd.rt.PublishOwn(peer); !errors.Is(err, dht.ErrNotPublishable) {
		t.Fatalf("own peer card: %v, want ErrNotPublishable", err)
	}
	// The same rule holds one level down, for whoever calls the DHT client directly.
	if err := nd.rt.DHT().Publish(peer); !errors.Is(err, dht.ErrNotPublishable) {
		t.Fatalf("DHT().Publish of a peer card: %v, want ErrNotPublishable", err)
	}
	if _, err := nd.rt.DHT().Put(context.Background(), peer); !errors.Is(err, dht.ErrNotPublishable) {
		t.Fatalf("DHT().Put of a peer card: %v, want ErrNotPublishable", err)
	}

	foreign := sealOwnHint(t, other, 1, raw.Now().Add(time.Hour))
	if err := nd.rt.PublishOwn(foreign); err == nil {
		t.Fatal("a record signed by another device was accepted")
	}
	otherPub, _ := other.rt.Cards().Own(objects.ScopePublic)
	if err := nd.rt.PublishOwn(otherPub); err == nil {
		t.Fatal("another node's card was accepted")
	}
	_, otherDev := ownDevices(t, other, raw.Now())
	if err := nd.rt.PublishOwn(otherDev); err == nil {
		t.Fatal("another account's device list was accepted")
	}
	if err := nd.rt.PublishOwn([]byte("garbage")); err == nil {
		t.Fatal("garbage was accepted")
	}
}

// A hidden node has no public card and publishes none; its device list is still its own record.
func TestHiddenNodePublishesNoCard(t *testing.T) {
	raw := &settableClock{now: time.Now()}
	clk, err := clock.New(clock.Config{Now: raw.Now})
	if err != nil {
		t.Fatal(err)
	}
	nd := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) { c.Clock = clk })
	nd.start(t)
	for _, rec := range node.OwnDHTRecords(nd.rt) {
		if _, err := objects.VerifyCard(rec, raw.Now()); err == nil {
			t.Fatal("a hidden node offers a card for the DHT")
		}
	}
	peer, ok := nd.rt.Cards().Own(objects.ScopePeer)
	if ok {
		if err := nd.rt.PublishOwn(peer); err == nil {
			t.Fatal("a hidden node's peer card was accepted")
		}
	}
}
