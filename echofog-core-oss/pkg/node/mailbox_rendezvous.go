package node

import (
	"context"
	"encoding/hex"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Mailbox rendezvous (see docs/design/conversation-log.md, section 6): the node keeps up to three
// relays it is connected to as its mailboxes and names them in its key bundle record, so a sender
// that cannot reach it knows where its mail is kept.

const (
	mailboxCount   = 3
	mailboxEvery   = 2 * time.Minute
	mailboxHintTTL = 6 * time.Hour
)

func (n *Node) mailboxLoop() {
	first := time.NewTimer(5 * time.Second)
	defer first.Stop()
	select {
	case <-n.Ctx.Done():
		return
	case <-first.C:
	}
	for {
		n.RefreshMailboxes()
		select {
		case <-n.Ctx.Done():
			return
		case <-time.After(mailboxEvery):
		}
	}
}

// isMailboxCandidate: a relay we may keep our mail on. Not somebody we chat with (a contact, or a
// stranger who wrote first): whether they are online is tied to the very conversations the
// mailbox stands in for.
func (n *Node) isMailboxCandidate(p *swarm.Peer) bool {
	if n.RelayClient != nil && n.RelayClient.IsPreferred(p.ID()) {
		return true
	}
	if !p.Relays() {
		return false
	}
	if n.Contact != nil && (n.Contact.Exists(p.ID().String()) || n.Contact.IsRequest(p.ID().String())) {
		return false
	}
	return true
}

// RefreshMailboxes chooses this node's mailbox relays now — the ones in use while they stay
// connected, then the longest-connected relays — and publishes them when they change or near
// their expiry.
func (n *Node) RefreshMailboxes() {
	if n.Swarm == nil {
		return
	}
	prev := n.Swarm.MyMailboxHints()
	now := time.Now()

	var candidates []*swarm.Peer
	byID := map[string]*swarm.Peer{}
	for _, p := range n.Swarm.GetAllPeers() {
		if n.isMailboxCandidate(p) {
			candidates = append(candidates, p)
			byID[p.ID().String()] = p
		}
	}
	chosen := make([]*swarm.Peer, 0, mailboxCount)
	taken := map[string]bool{}
	for _, h := range prev {
		if p := byID[h.Peer]; p != nil && len(chosen) < mailboxCount && !taken[h.Peer] {
			chosen, taken[h.Peer] = append(chosen, p), true
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ConnectedAt().Before(candidates[j].ConnectedAt())
	})
	for _, p := range candidates {
		if len(chosen) == mailboxCount {
			break
		}
		if id := p.ID().String(); !taken[id] {
			chosen, taken[id] = append(chosen, p), true
		}
	}

	same := len(chosen) == len(prev)
	for i := 0; same && i < len(chosen); i++ {
		same = chosen[i].ID().String() == prev[i].Peer && prev[i].Until-now.Unix() > int64(mailboxHintTTL.Seconds()/2)
	}
	if same {
		return
	}
	hints := make([]swarm.MailboxHint, 0, len(chosen))
	for _, p := range chosen {
		hints = append(hints, swarm.MailboxHint{Peer: p.ID().String(), Addr: p.Addr(), Until: now.Add(mailboxHintTTL).Unix()})
	}
	n.Swarm.SetMailboxHints(hints)
	n.publishBundleNow()
}

// OnMessengerReady runs f during Start, right after the messenger is made and before it receives
// anything, so a chat service that hooks into it misses no early message. A hook added after Start
// is not run.
func (n *Node) OnMessengerReady(f func()) {
	n.hookMu.Lock()
	n.messengerHooks = append(n.messengerHooks, f)
	n.hookMu.Unlock()
}

func (n *Node) publishBundleNow() {
	select {
	case n.bundleNow <- struct{}{}:
	default:
	}
}

// reachMailboxes connects to the relays that keep peer's mail (asking the network for the peer's
// current record if we know none), then hands them what waits for the peer.
func (n *Node) reachMailboxes(ctx context.Context, peer types.PeerID) {
	hints := n.Swarm.MailboxHintsOf(peer)
	if len(hints) == 0 {
		hints = n.Swarm.RefreshMailboxHints(peer)
	}
	reached := false
	for _, h := range hints {
		raw, err := hex.DecodeString(h.Peer)
		if err != nil || len(raw) != len(types.PeerID{}) {
			continue
		}
		var id types.PeerID
		copy(id[:], raw)
		if id == n.ID {
			continue
		}
		if n.Swarm.GetPeer(id) != nil {
			reached = true
			continue
		}
		if err := n.EnsureConnected(ctx, id); err != nil && h.Addr != "" {
			// through the swarm: a relay (a server) whose camouflage SNI we know is dialed with it,
			// and one we do not know it for is not dialed over noise across a border
			_ = n.Swarm.AddPotentialPeerWithCtx(ctx, id, h.Addr)
		}
		if n.Swarm.GetPeer(id) != nil {
			reached = true
		}
	}
	if reached && n.Messenger != nil {
		n.Messenger.FlushOutbox(peer)
	}
}
