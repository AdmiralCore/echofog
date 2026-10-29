package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Who may reach this node (see config.AccessConfig and package access). The access secret, the
// peers who joined with it and the secrets of the hidden nodes this node was invited to are kept
// in the sealed part of the database ("vpnmarket:" values are encrypted).
const (
	accessStateKey = "vpnmarket:access"
	invitesKey     = "vpnmarket:invites"
	linkRelays     = 2 // relays named in a link of a hidden node
)

type accessState struct {
	Secret string           `json:"secret,omitempty"`
	Joined map[string]int64 `json:"joined,omitempty"` // peer ID -> unix time of joining
}

var accessSaveMu sync.Mutex

// AccessMode is how this node lets others reach it: config.AccessPublic, AccessHidden or
// AccessIsolated. It is fixed when the node starts; a change of the setting takes effect at the
// next start.
func (n *Node) AccessMode() string {
	if m := n.accessMode.Load(); m != nil {
		return *m
	}
	if n.Cfg == nil || n.Cfg.Access.Mode == "" {
		return config.AccessPublic
	}
	return n.Cfg.Access.Mode
}

func (n *Node) gated() bool {
	m := n.AccessMode()
	return m == config.AccessHidden || m == config.AccessIsolated
}

func (n *Node) isolated() bool { return n.AccessMode() == config.AccessIsolated }

// initAccess loads the access state from the sealed storage: the gate (who may reach this node)
// and the keyring (the hidden nodes it was invited to). Called once the storage is open.
func (n *Node) initAccess() error {
	mode := n.Cfg.Access.Mode
	switch mode {
	case "":
		mode = config.AccessPublic
	case config.AccessPublic, config.AccessHidden, config.AccessIsolated:
	default:
		return fmt.Errorf("unknown access mode %q (public, hidden or isolated)", mode)
	}
	n.accessMode.Store(&mode)
	n.Access = access.NewGate()
	n.Invites = access.NewKeyring()

	var st accessState
	if raw, err := n.Storage.Get([]byte(accessStateKey)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &st)
	}
	var secret []byte
	if s := n.Cfg.Access.Secret; s != "" {
		b, err := access.DecodeSecret(s)
		if err != nil {
			return errors.New("access secret must be 22 characters of base64url (16 bytes)")
		}
		secret = b
	} else if b, err := access.DecodeSecret(st.Secret); err == nil {
		secret = b
	} else if n.gated() {
		secret = access.NewSecret()
	}
	if secret != nil {
		_ = n.Access.SetSecret(secret)
	}
	joined := make(map[types.PeerID]int64, len(st.Joined))
	for s, at := range st.Joined {
		if id, err := types.PeerIDFromString(s); err == nil {
			joined[id] = at
		}
	}
	n.Access.SetJoined(joined)
	n.Access.OnJoin(func(id types.PeerID) {
		n.Logger.Info("access: a peer joined with the invitation link", "peer", id.String()[:8])
		_ = n.saveAccess()
	})
	// the owner's contacts get in without a link: calls and messages keep working
	n.Access.SetAllow(n.IsContact) // not a stranger who wrote once, not somebody blocked
	n.Access.Enable(n.gated())
	if n.gated() && n.Cfg.Access.Secret == "" && st.Secret == "" {
		if err := n.saveAccess(); err != nil {
			return fmt.Errorf("saving the access secret: %w", err)
		}
	}

	if raw, err := n.Storage.Get([]byte(invitesKey)); err == nil && len(raw) > 0 {
		var keys map[string]string
		if json.Unmarshal(raw, &keys) == nil {
			for s, k := range keys {
				id, err1 := types.PeerIDFromString(s)
				secret, err2 := access.DecodeSecret(k)
				if err1 == nil && err2 == nil {
					_ = n.Invites.Set(id, secret)
				}
			}
		}
	}
	return nil
}

// applyAccess hands the gate and the keyring to the transports and the exit. A gated node also
// stops announcing its port, so the peers it dials do not spread its address.
func (n *Node) applyAccess() {
	hide := n.gated()
	n.Net.SetAccess(n.Access, n.Invites, hide)
	if n.WS != nil {
		n.WS.SetAccess(n.Access, n.Invites, hide)
	}
	if hide {
		n.ExitService.SetAccess(n.Access.Allowed)
		n.ExitService.SetMeter(n.Passes.Charge) // a pass's traffic allowance
	}
	if n.isolated() {
		n.Net.SetNoDial(true)
		n.Swarm.SetNoDial(true) // also for WebSocket peers
	}
}

func (n *Node) saveAccess() error {
	if n.Storage == nil || n.Access == nil {
		return errors.New("node is not running")
	}
	accessSaveMu.Lock()
	defer accessSaveMu.Unlock()
	st := accessState{Joined: map[string]int64{}}
	// a secret given in the config stays there: it is not copied into the profile
	if s := n.Access.Secret(); s != nil && n.Cfg.Access.Secret == "" {
		st.Secret = access.EncodeSecret(s)
	}
	for id, at := range n.Access.Joined() {
		st.Joined[id.String()] = at
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return n.Storage.Set([]byte(accessStateKey), raw)
}

func (n *Node) saveInvites() error {
	if n.Storage == nil || n.Invites == nil {
		return errors.New("node is not running")
	}
	accessSaveMu.Lock()
	defer accessSaveMu.Unlock()
	keys := map[string]string{}
	for id, s := range n.Invites.All() {
		keys[id.String()] = access.EncodeSecret(s)
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return n.Storage.Set([]byte(invitesKey), raw)
}

// RotateAccessSecret replaces the access secret: links given out so far stop letting new people
// in, while those who already joined stay. It returns the new link.
func (n *Node) RotateAccessSecret() (string, error) {
	if n.Access == nil {
		return "", errors.New("node is not running")
	}
	if n.Cfg.Access.Secret != "" {
		return "", errors.New("the access secret is set in the node's configuration: change it there")
	}
	_ = n.Access.SetSecret(access.NewSecret())
	if err := n.saveAccess(); err != nil {
		return "", err
	}
	return n.InviteLink()
}

// RemoveJoined takes one peer off the list of those who joined: it needs a current link to come back.
func (n *Node) RemoveJoined(id types.PeerID) error {
	if n.Access == nil {
		return errors.New("node is not running")
	}
	if !n.Access.Remove(id) {
		return errors.New("this peer has not joined")
	}
	if n.gated() && !n.Access.Allowed(id) && n.Swarm.GetPeer(id) != nil {
		n.Swarm.RemovePeer(id)
	}
	return n.saveAccess()
}

// InviteLink is the link others use to reach this node: its addresses and, when it is hidden or
// isolated, its access secret.
func (n *Node) InviteLink() (string, error) {
	if n.Access == nil {
		return "", errors.New("node is not running")
	}
	var secret []byte
	if n.gated() {
		secret = n.Access.Secret()
	}
	return n.inviteLinkWith(secret)
}

// inviteLinkWith is the invitation link carrying secret: the shared one, or a pass's.
func (n *Node) inviteLinkWith(secret []byte) (string, error) {
	if n.Access == nil || n.Swarm == nil {
		return "", errors.New("node is not running")
	}
	inv := access.Invite{
		Network:  config.NetworkName,
		Protocol: n.Cfg.Network.ProtocolVersion,
		Peer:     n.ID,
		Addrs:    n.linkAddrs(),
		SNI:      n.Cfg.AntiDPI.TLSServerName,
		Server:   n.Cfg.IsServer(),
	}
	if n.gated() {
		inv.Secret = secret
	}
	// relays help to reach a hidden node, or one with no address yet; a public node with an
	// address is dialed there, and naming whatever relays it happens to be connected to would hand
	// out other people's nodes with its link
	if !n.isolated() && (n.gated() || len(inv.Addrs) == 0) {
		inv.Relays = n.linkRelays()
	}
	if len(inv.Addrs) == 0 && len(inv.Relays) == 0 {
		return "", errors.New("this node does not know an address others can reach it at yet: wait until it is connected, or set the public address")
	}
	return access.FormatLink(inv), nil
}

// linkAddrs are the addresses put in a link: the configured ones, else what the network observes
// (NAT discovery, the router's port mapping), else the public addresses of this machine. With an
// advertised port (Docker publishing host 443 to the container's 4242) the observed IP goes with
// that port: STUN sees the port the container's NAT picked for outgoing packets, which nobody can
// dial.
func (n *Node) linkAddrs() []string {
	if len(n.Cfg.Access.PublicAddrs) > 0 {
		return append([]string(nil), n.Cfg.Access.PublicAddrs...)
	}
	seen := map[string]bool{}
	var out []string
	adv := n.Cfg.Network.AdvertisePort
	add := func(a string) {
		if host, _, err := net.SplitHostPort(a); err == nil && adv > 0 {
			a = net.JoinHostPort(host, strconv.Itoa(adv))
		}
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if n.NAT != nil {
		for _, a := range n.NAT.Candidates() {
			add(a)
		}
	}
	if len(out) == 0 && n.Transport != nil {
		port := n.Transport.UDPPort()
		ifaces, _ := net.InterfaceAddrs()
		for _, ia := range ifaces {
			ipn, ok := ia.(*net.IPNet)
			if ok && ipn.IP.IsGlobalUnicast() && !ipn.IP.IsPrivate() {
				add(net.JoinHostPort(ipn.IP.String(), strconv.Itoa(port)))
			}
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// linkRelays are relays this node is connected to, through which a peer can ask for a hole punch
// when no address of this node can be dialed.
func (n *Node) linkRelays() []string {
	var out []string
	for _, p := range n.Swarm.GetAllPeers() {
		if len(out) >= linkRelays {
			break
		}
		if (p.Relays() || p.Signals()) && p.Addr() != "" {
			// the relay's SNI and role go with it: whoever follows the link dials it as we do
			out = append(out, network.JoinPeerAddrInfo(p.Addr(), p.ID(), n.Swarm.GetPeerSNI(p.ID()), n.Swarm.IsServerPeer(p.ID())))
		}
	}
	return out
}

// JoinInvite stores the secret of an invitation and connects to the node it names: directly when
// one of its addresses answers, else through one of its relays with a hole punch. It returns the
// node's peer ID.
func (n *Node) JoinInvite(ctx context.Context, inv access.Invite) (types.PeerID, error) {
	if n.Invites == nil || n.Swarm == nil {
		return types.PeerID{}, errors.New("node is not running")
	}
	if inv.Peer == n.ID {
		return types.PeerID{}, errors.New("this is your own node")
	}
	if inv.Protocol > 0 && inv.Protocol < handshake.MinProtocolVersion {
		return types.PeerID{}, errors.New("the node runs an outdated version: ask its owner to update it")
	}
	if inv.Secret != nil {
		if err := n.Invites.Set(inv.Peer, inv.Secret); err != nil {
			return types.PeerID{}, err
		}
		if err := n.saveInvites(); err != nil {
			return types.PeerID{}, err
		}
	}
	n.Swarm.MarkExplicitPeer(inv.Peer) // the user chose this node: it is linked to directly, not through relays
	if inv.Server {
		n.Swarm.SetPeerServer(inv.Peer, true)
	}
	if inv.SNI != "" {
		n.Swarm.SetPeerSNI(inv.Peer, inv.SNI) // remembered: every later dial to it is camouflaged too
		for _, a := range inv.Addrs {
			n.Net.ExpectPeerSNI(ResolveRelayHost(a), inv.Peer, inv.SNI)
		}
	}
	if n.Swarm.GetPeer(inv.Peer) != nil {
		return inv.Peer, nil
	}
	var lastErr error
	for _, a := range inv.Addrs {
		dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		lastErr = n.Swarm.AddPotentialPeerWithCtx(dctx, inv.Peer, ResolveRelayHost(a))
		cancel()
		if lastErr == nil || n.Swarm.GetPeer(inv.Peer) != nil {
			return inv.Peer, nil
		}
	}
	for _, r := range inv.Relays {
		rinfo := network.ParseAddrInfo(r)
		addr, relay, ok := rinfo.Addr, rinfo.ID, rinfo.Named
		if !ok || relay == n.ID {
			continue
		}
		if rinfo.SNI != "" {
			n.Swarm.SetPeerSNI(relay, rinfo.SNI)
		}
		if rinfo.Server {
			n.Swarm.SetPeerServer(relay, true)
		}
		if n.Swarm.GetPeer(relay) == nil {
			dctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			err := n.Swarm.AddPotentialPeerWithCtx(dctx, relay, ResolveRelayHost(addr))
			cancel()
			if err != nil {
				lastErr = err
				continue
			}
		}
		n.RelayClient.AddPreferred(relay)
		pctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		lastErr = n.connectToPeer(pctx, inv.Peer, "")
		cancel()
		if lastErr == nil || n.Swarm.GetPeer(inv.Peer) != nil {
			return inv.Peer, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no address in the link answered")
	}
	return types.PeerID{}, fmt.Errorf("could not reach the node: %w", lastErr)
}

// ForgetInvite drops the stored secret of a hidden node.
func (n *Node) ForgetInvite(id types.PeerID) error {
	if n.Invites == nil {
		return errors.New("node is not running")
	}
	_ = n.Invites.Set(id, nil)
	return n.saveInvites()
}

// admitPunch decides whether to answer a hole punch request: a hidden node answers (and so gives
// out its addresses) only to the peers it lets in.
func (n *Node) admitPunch(from types.PeerID, ticket []byte) bool {
	if n.ProxyOn() {
		return n.admitPunchBehindProxy(from)
	}
	if n.Access == nil || !n.Access.Enabled() {
		// guard mode: no hole punch with a user across the border; the traffic goes through relays
		return !n.Swarm.UserLinkViaRelay(from, "")
	}
	return n.Access.AdmitTicket(from, ticket)
}
