package node

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// AccessStatus is who may reach this node and the link that lets people in.
type AccessStatus struct {
	// Mode is the mode the node runs in: "public", "hidden" or "isolated".
	Mode string `json:"mode"`
	// NextMode is the mode saved for the next start, when it differs from Mode.
	NextMode string `json:"nextMode,omitempty"`
	// Link is the invitation link ("" while the node knows no address of its own yet).
	Link string `json:"link"`
	// WebLink is the same link as an https page that messengers make tappable (the EchoFog link
	// rides after "#", which never reaches the page's server).
	WebLink string `json:"webLink"`
	// LinkError says why there is no link yet.
	LinkError string `json:"linkError,omitempty"`
	// SecretFromConfig: the secret is set in the node's configuration and cannot be rotated here.
	SecretFromConfig bool         `json:"secretFromConfig"`
	Joined           []JoinedPeer `json:"joined"`
	Network          string       `json:"network"`
}

// JoinedPeer is somebody who came in with the link.
type JoinedPeer struct {
	PeerID    string `json:"peerId"`
	JoinedAt  int64  `json:"joinedAt"`
	Connected bool   `json:"connected"`
}

// GetAccessStatus returns the access mode, the invitation link and who joined with it.
func (n *NodeApi) GetAccessStatus() (AccessStatus, error) {
	if n.node == nil || n.node.Access == nil {
		return AccessStatus{}, errors.New("node is not running")
	}
	st := AccessStatus{
		Mode:             n.node.AccessMode(),
		SecretFromConfig: n.node.Cfg.Access.Secret != "",
		Network:          config.NetworkName,
		Joined:           []JoinedPeer{},
	}
	settingsMu.Lock()
	next := n.nextAccessMode
	settingsMu.Unlock()
	if next != "" && next != st.Mode {
		st.NextMode = next
	}
	if link, err := n.node.InviteLink(); err == nil {
		st.Link, st.WebLink = link, access.WebLink(link)
	} else {
		st.LinkError = err.Error()
	}
	for id, at := range n.node.Access.Joined() {
		st.Joined = append(st.Joined, JoinedPeer{PeerID: id.String(), JoinedAt: at, Connected: n.node.Swarm.GetPeer(id) != nil})
	}
	sort.Slice(st.Joined, func(i, j int) bool { return st.Joined[i].JoinedAt > st.Joined[j].JoinedAt })
	return st, nil
}

// SetAccessMode saves the access mode ("public" or "hidden") for the next start of the node, or
// for the coming one when the node has not started yet.
// Isolated is for servers only and is set in the node's configuration.
func (n *NodeApi) SetAccessMode(mode string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node is not running")
	}
	if mode != config.AccessPublic && mode != config.AccessHidden {
		return errors.New("access mode must be public or hidden")
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	if err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.AccessMode = mode }); err != nil {
		return err
	}
	n.nextAccessMode = mode
	if n.node.Swarm == nil {
		// chosen before the node first starts (the welcome screen after registering): it applies
		// to this very start, not only the next one
		n.node.Cfg.Access.Mode = mode
	}
	return nil
}

// RotateAccessSecret makes a new link: the old one stops letting new people in; those who joined
// with it stay until removed.
func (n *NodeApi) RotateAccessSecret() (string, error) {
	if n.node == nil {
		return "", errors.New("node is not running")
	}
	return n.node.RotateAccessSecret()
}

// RemoveJoinedPeer takes somebody off the list of those who joined with the link.
func (n *NodeApi) RemoveJoinedPeer(peerID string) error {
	if n.node == nil {
		return errors.New("node is not running")
	}
	id, err := types.PeerIDFromString(peerID)
	if err != nil {
		return errors.New("invalid peer id")
	}
	return n.node.RemoveJoined(id)
}

// InviteInfo is what an invitation link says, for the app to show before connecting.
type InviteInfo struct {
	PeerID   string `json:"peerId"`
	Private  bool   `json:"private"` // the link carries an access secret (a hidden or isolated node)
	Addrs    int    `json:"addrs"`
	Relays   int    `json:"relays"`
	Protocol uint32 `json:"protocol"`
	Server   bool   `json:"server"`        // the node is a server (a relay or exit on a VPS)
	SNI      string `json:"sni,omitempty"` // the site the node's traffic looks like
}

// ParseInvite reads an invitation link. It tells a link of another network (a fork) and one made
// by a newer app apart from a broken one.
func (n *NodeApi) ParseInvite(link string) (InviteInfo, error) {
	inv, err := access.ParseLink(link, config.NetworkName)
	if err != nil {
		return InviteInfo{}, err
	}
	return InviteInfo{PeerID: inv.Peer.String(), Private: inv.Secret != nil, Addrs: len(inv.Addrs), Relays: len(inv.Relays),
		Protocol: inv.Protocol, Server: inv.Server, SNI: inv.SNI}, nil
}

// JoinInvite stores the invitation and connects to the node it names; it returns the node's peer ID.
func (n *NodeApi) JoinInvite(link string) (string, error) {
	if n.node == nil || n.node.Swarm == nil {
		return "", errors.New("node is not running")
	}
	inv, err := access.ParseLink(link, config.NetworkName)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(n.node.Ctx, 60*time.Second)
	defer cancel()
	id, err := n.node.JoinInvite(ctx, inv)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// ForgetInvite drops the stored invitation to a hidden node.
func (n *NodeApi) ForgetInvite(peerID string) error {
	if n.node == nil {
		return errors.New("node is not running")
	}
	id, err := types.PeerIDFromString(peerID)
	if err != nil {
		return errors.New("invalid peer id")
	}
	return n.node.ForgetInvite(id)
}

// linkEntry turns the invitation link of a public node into the address entry the relay and
// bootstrap lists keep, host:port/<peer id> with its SNI and role; anything else is returned as it
// is. A hidden node's link is joined, not listed.
func linkEntry(link string) (string, error) {
	link = strings.TrimSpace(link)
	if !access.IsLink(link) {
		return link, nil
	}
	inv, err := access.ParseLink(link, config.NetworkName)
	if err != nil {
		return "", err
	}
	if inv.Secret != nil {
		return "", errors.New("this is an invitation to a hidden node: join it instead")
	}
	if len(inv.Addrs) == 0 {
		return "", errors.New("the link names no address to reach the node at")
	}
	return network.JoinPeerAddrInfo(inv.Addrs[0], inv.Peer, inv.SNI, inv.Server), nil
}
