package node

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// PublishNamespace is the store namespace of the node's own publication state (AD13): the issued
// device list under the key "devices".
const PublishNamespace = "v2/publish"

const devicesKey = "devices"

// deviceListRenewBefore is how long before the end of its lifetime the device list is issued
// again (objects.md §6: republished when less than 10 days remain).
const deviceListRenewBefore = 10 * 24 * time.Hour

// ErrNotListed is PublishOwn on a node without a public listing (see listed).
var ErrNotListed = errors.New("node: the node is not publicly listed, nothing of it goes into the DHT")

// ErrNotOwnRecord is a record PublishOwn refuses: not signed by this node's device (or, for
// devices/2, by its account) or not one of the labels a node publishes about itself.
var ErrNotOwnRecord = errors.New("node: not an own record that a node publishes")

// ownDevices keeps the issued device list in memory and in the store.
type ownDevices struct {
	mu  sync.Mutex
	raw []byte
}

// PublishOwn registers an own object for storing in the DHT and for republication: the single
// helper for card/2, devices/2, prekeys/2 and mailbox-hint/2 (T4.2b). Anything else, anything
// signed by another key and any card that is not public is refused, so a service cannot put a
// peer card (it carries the noise3 masking keys) or somebody else's record into the DHT.
func (r *Runtime) PublishOwn(raw []byte) error {
	if !r.listed() {
		return ErrNotListed
	}
	env, err := objects.Verify(raw, r.carrierClock.Now())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotOwnRecord, err)
	}
	switch env.Label {
	case objects.LabelCard, objects.LabelPrekeys, objects.LabelMailboxHint:
		if env.SignerID() != [32]byte(r.identity.DeviceID) {
			return ErrNotOwnRecord
		}
	case objects.LabelDevices:
		if env.SignerID() != [32]byte(r.identity.NodeID) {
			return ErrNotOwnRecord
		}
	default:
		return ErrNotOwnRecord
	}
	cl := r.DHT()
	if cl == nil {
		return errors.New("node: the DHT client is not running")
	}
	return cl.Publish(raw)
}

// currentDeviceList returns the account's device list for the DHT: the one kept in the store while
// at least 10 days of its life remain, otherwise a new one. Its seq follows the clock (never below
// the previous one plus one), so a lost store cannot make a later list older than one that readers
// have already seen.
func (r *Runtime) currentDeviceList(now time.Time) ([]byte, error) {
	d := &r.devList
	d.mu.Lock()
	defer d.mu.Unlock()

	var kv KV
	if r.cfg.Store != nil {
		kv = r.cfg.Store.Store(PublishNamespace)
	}
	if d.raw == nil && kv != nil {
		if b, err := kv.Get([]byte(devicesKey)); err == nil {
			d.raw = b
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	var prev uint64
	if d.raw != nil {
		dl, err := objects.VerifyDeviceList(d.raw, now)
		if err == nil && dl.NodeID == [32]byte(r.identity.NodeID) && dl.Contains(r.cfg.Device.Public()) {
			prev = dl.Env.Seq
			if time.Unix(int64(dl.Env.NotAfter), 0).Sub(now) >= deviceListRenewBefore {
				return d.raw, nil
			}
		}
	}
	seq := uint64(now.Unix())
	if seq <= prev {
		seq = prev + 1
	}
	raw, err := r.cfg.Account.SealSingleDeviceList(r.cfg.Device, seq, now)
	if err != nil {
		return nil, err
	}
	if kv != nil {
		if err := kv.Put([]byte(devicesKey), raw); err != nil {
			return nil, err
		}
	}
	d.raw = raw
	return raw, nil
}

// listed reports whether the node has a public listing: a visible node with a public card.
// dht.md §7: records of hidden or unlisted nodes are never published; that includes the device
// list, which would tie the node ID to the device key, and the hints of a node nobody may find.
// Hidden and anonymous nodes and a visible node without a camouflaged listener (D37) are not
// listed. Visibility is read from the card builder each time, so a change takes effect at once.
func (r *Runtime) listed() bool {
	if r.cfg.Cards.Visibility() != cards.Visible {
		return false
	}
	_, ok := r.cards.Own(objects.ScopePublic)
	return ok
}
