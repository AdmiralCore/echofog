package node

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Passes are the personal invitations of a hidden or isolated node (see access.Passbook): an owner
// who takes payment their own way makes one per customer, extends it when they pay again and
// revokes it to take them off. They are kept in the sealed part of the database.
const passesKey = "vpnmarket:passes"

// passSweepEvery is how often the node drops the devices of passes that ended and saves the
// traffic counts.
var passSweepEvery = 30 * time.Second

// ErrPassesNeedHidden: passes only mean something on a node that does not let everybody in.
var ErrPassesNeedHidden = errors.New("passes need a hidden or isolated node: set ECHOFOG_ACCESS=hidden or isolated")

func (n *Node) initPasses() {
	n.Passes = access.NewPassbook()
	if raw, err := n.Storage.Get([]byte(passesKey)); err == nil && len(raw) > 0 {
		var list []access.Pass
		if json.Unmarshal(raw, &list) == nil {
			n.Passes.Load(list)
		}
	}
	n.Passes.OnChange(func(structural bool) {
		n.passesDirty.Store(true)
		if structural {
			if err := n.savePasses(); err != nil {
				n.Logger.Warn("passes: could not save", "err", err)
			}
		}
	})
	n.Access.SetPasses(n.Passes)
}

func (n *Node) savePasses() error {
	if n.Storage == nil || n.Passes == nil {
		return errors.New("node is not running")
	}
	accessSaveMu.Lock()
	defer accessSaveMu.Unlock()
	n.passesDirty.Store(false)
	raw, err := json.Marshal(n.Passes.List())
	if err != nil {
		return err
	}
	return n.Storage.Set([]byte(passesKey), raw)
}

// sweepPasses runs while the node does: it drops the connections of devices whose pass no longer
// lets them in, and saves the traffic counts now and then.
func (n *Node) sweepPasses() {
	t := time.NewTicker(passSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-n.Ctx.Done():
			return
		case <-t.C:
		}
		n.shutPasses()
		if n.passesDirty.Load() {
			_ = n.savePasses()
		}
	}
}

// shutPasses drops the connections of the devices whose pass ended, ran out or was revoked.
func (n *Node) shutPasses() {
	if n.Passes == nil || n.Swarm == nil || !n.gated() {
		return
	}
	for _, id := range n.Passes.Shut() {
		if !n.Access.Allowed(id) && n.Swarm.GetPeer(id) != nil {
			n.Swarm.RemovePeer(id)
		}
	}
}

// PassView is a pass as the owner sees it: its link and state, with the secret left out.
type PassView struct {
	ID         string       `json:"id"`
	Label      string       `json:"label,omitempty"`
	Link       string       `json:"link"`
	State      string       `json:"state"` // active, expired, used_up, revoked
	Created    int64        `json:"created"`
	Expires    int64        `json:"expires,omitempty"`
	MaxDevices int          `json:"maxDevices"`
	QuotaBytes int64        `json:"quotaBytes,omitempty"`
	UsedBytes  int64        `json:"usedBytes"`
	Devices    []PassDevice `json:"devices"`
}

// PassDevice is one device bound to a pass.
type PassDevice struct {
	PeerID    string `json:"peerId"`
	JoinedAt  int64  `json:"joinedAt"`
	Connected bool   `json:"connected"`
}

func (n *Node) passView(p access.Pass) PassView {
	v := PassView{ID: p.ID, Label: p.Label, State: p.State(time.Now()), Created: p.Created, Expires: p.Expires,
		MaxDevices: p.MaxDevices, QuotaBytes: p.QuotaBytes, UsedBytes: p.UsedBytes, Devices: []PassDevice{}}
	v.Link, _ = n.inviteLinkWith(p.Secret)
	for s, at := range p.Devices {
		d := PassDevice{PeerID: s, JoinedAt: at}
		if id, err := types.PeerIDFromString(s); err == nil && n.Swarm != nil {
			d.Connected = n.Swarm.GetPeer(id) != nil
		}
		v.Devices = append(v.Devices, d)
	}
	return v
}

func (n *Node) passesReady() error {
	if n.Passes == nil {
		return errors.New("node is not running")
	}
	if !n.gated() {
		return ErrPassesNeedHidden
	}
	return nil
}

// ListPasses returns every pass, newest first.
func (n *Node) ListPasses() ([]PassView, error) {
	if err := n.passesReady(); err != nil {
		return nil, err
	}
	out := []PassView{}
	for _, p := range n.Passes.List() {
		out = append(out, n.passView(p))
	}
	return out, nil
}

// GetPass returns one pass.
func (n *Node) GetPass(id string) (PassView, error) {
	if err := n.passesReady(); err != nil {
		return PassView{}, err
	}
	p, err := n.Passes.Get(id)
	if err != nil {
		return PassView{}, err
	}
	return n.passView(p), nil
}

// CreatePass makes a pass and returns it with its link.
func (n *Node) CreatePass(s access.PassSpec) (PassView, error) {
	if err := n.passesReady(); err != nil {
		return PassView{}, err
	}
	p, err := n.Passes.Create(s)
	if err != nil {
		return PassView{}, err
	}
	n.Logger.Info("passes: made a pass", "id", p.ID, "label", p.Label)
	return n.passView(p), nil
}

// ExtendPass renews a pass (see access.Passbook.Extend).
func (n *Node) ExtendPass(id string, s access.PassSpec, resetUsage bool) (PassView, error) {
	if err := n.passesReady(); err != nil {
		return PassView{}, err
	}
	p, err := n.Passes.Extend(id, s, resetUsage)
	if err != nil {
		return PassView{}, err
	}
	return n.passView(p), nil
}

// RevokePass stops a pass and drops its devices at once.
func (n *Node) RevokePass(id string) (PassView, error) {
	if err := n.passesReady(); err != nil {
		return PassView{}, err
	}
	p, err := n.Passes.Revoke(id)
	if err != nil {
		return PassView{}, err
	}
	n.shutPasses()
	return n.passView(p), nil
}

// DeletePass forgets a pass and drops its devices.
func (n *Node) DeletePass(id string) error {
	if err := n.passesReady(); err != nil {
		return err
	}
	p, err := n.Passes.Get(id)
	if err != nil {
		return err
	}
	if err := n.Passes.Delete(id); err != nil {
		return err
	}
	for s := range p.Devices {
		if pid, err := types.PeerIDFromString(s); err == nil && !n.Access.Allowed(pid) && n.Swarm.GetPeer(pid) != nil {
			n.Swarm.RemovePeer(pid)
		}
	}
	return nil
}

// RemovePassDevice takes one device off a pass and drops it, freeing a place on the pass.
func (n *Node) RemovePassDevice(id string, peer types.PeerID) error {
	if err := n.passesReady(); err != nil {
		return err
	}
	if err := n.Passes.RemoveDevice(id, peer); err != nil {
		return err
	}
	if !n.Access.Allowed(peer) && n.Swarm.GetPeer(peer) != nil {
		n.Swarm.RemovePeer(peer)
	}
	return nil
}
