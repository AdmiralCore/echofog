package node

import (
	"errors"

	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// Passes of a hidden or isolated node: personal invitation links with a term, a number of devices
// and an optional traffic allowance, for an owner who takes payment their own way. See
// docs/operator-api.md.

// PassView is a pass as its owner sees it.
type PassView = coreNode.PassView

// PassRequest makes or extends a pass. On a new pass Days 0 means it never ends, Devices 0 means
// two and GB 0 means no traffic limit; when extending, the Days and GB are added and Devices and
// Label change only when set.
type PassRequest struct {
	Label      string  `json:"label,omitempty"`
	Days       float64 `json:"days,omitempty"`
	Devices    int     `json:"devices,omitempty"`
	GB         float64 `json:"gb,omitempty"`
	ResetUsage bool    `json:"resetUsage,omitempty"` // extending: count the traffic from zero again
}

func (r PassRequest) spec() access.PassSpec {
	return access.PassSpec{Label: r.Label, Days: r.Days, MaxDevices: r.Devices, QuotaGB: r.GB}
}

func (n *NodeApi) passNode() (*coreNode.Node, error) {
	if n.node == nil {
		return nil, errors.New("node not started")
	}
	return n.node, nil
}

// ListPasses returns every pass, newest first.
func (n *NodeApi) ListPasses() ([]PassView, error) {
	nd, err := n.passNode()
	if err != nil {
		return nil, err
	}
	return nd.ListPasses()
}

// GetPass returns one pass.
func (n *NodeApi) GetPass(id string) (PassView, error) {
	nd, err := n.passNode()
	if err != nil {
		return PassView{}, err
	}
	return nd.GetPass(id)
}

// CreatePass makes a pass and returns it with its link.
func (n *NodeApi) CreatePass(r PassRequest) (PassView, error) {
	nd, err := n.passNode()
	if err != nil {
		return PassView{}, err
	}
	return nd.CreatePass(r.spec())
}

// ExtendPass renews a pass: after a payment, add days or traffic; it also lifts a revocation.
func (n *NodeApi) ExtendPass(id string, r PassRequest) (PassView, error) {
	nd, err := n.passNode()
	if err != nil {
		return PassView{}, err
	}
	return nd.ExtendPass(id, r.spec(), r.ResetUsage)
}

// RevokePass stops a pass and drops its devices; ExtendPass lets them in again.
func (n *NodeApi) RevokePass(id string) (PassView, error) {
	nd, err := n.passNode()
	if err != nil {
		return PassView{}, err
	}
	return nd.RevokePass(id)
}

// DeletePass forgets a pass and drops its devices.
func (n *NodeApi) DeletePass(id string) error {
	nd, err := n.passNode()
	if err != nil {
		return err
	}
	return nd.DeletePass(id)
}

// RemovePassDevice takes one device off a pass, freeing its place.
func (n *NodeApi) RemovePassDevice(id, peerID string) error {
	nd, err := n.passNode()
	if err != nil {
		return err
	}
	peer, err := types.PeerIDFromString(peerID)
	if err != nil {
		return errors.New("bad peer ID")
	}
	return nd.RemovePassDevice(id, peer)
}
