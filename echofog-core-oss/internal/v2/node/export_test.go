package node

// Test seams: the runtime keeps these internals unexported; the external tests use them to check
// that the platform flags and typed events are actually wired, without widening the public API.

import (
	"context"
	"errors"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"net"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/portmap"
	"github.com/echofog/echofog-core/internal/v2/punch"
	"github.com/echofog/echofog-core/internal/v2/session"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

var (
	// SetHealthy replaces the check that a session has a working path again.
	SetHealthy = func(r *Runtime, f func(*mux.Session) bool) { r.healthyFn = f }
	// RepairCardOf is the card a repair of the session would dial with.
	RepairCardOf = func(r *Runtime, sid [16]byte) *objects.Card { return r.repairCard(sid) }
	// TriggerPathFailing runs the reaction to a session left with one failing path.
	TriggerPathFailing = func(r *Runtime, s *mux.Session) { r.pathFailing(s, mux.PathFailure{}) }
	// SetRepairOpt sets what a repair dials with.
	SetRepairOpt = func(r *Runtime, opt DialOpt) { r.repairOpt = opt }
	// OpenSessions counts the live (not closed) sessions in the runtime's session table.
	OpenSessions = func(r *Runtime) int {
		n := 0
		for _, est := range r.table.Sessions() {
			select {
			case <-est.Session.Done():
			default:
				n++
			}
		}
		return n
	}
	// LimitsOf returns the pre-authentication limits of the accept path (handshake 11).
	LimitsOf = func(r *Runtime) *session.Limits { return r.limits }
	// ObserverOf returns the runtime's own-address observer (wire.md §7.2).
	ObserverOf = func(r *Runtime) *session.Observer { return r.observer }
	// SetBeforeAdd installs the hook that runs before an accepted session enters the table.
	SetBeforeAdd = func(f func(*session.Established)) {
		if f == nil {
			beforeAdd.Store(nil)
			return
		}
		beforeAdd.Store(&f)
	}
	// SetBeforeServe installs the hook that runs in ensureServing before the goroutine is counted (stage 0) and inside it (stage 1).
	SetBeforeServe = func(f func(stage int)) {
		if f == nil {
			beforeServe.Store(nil)
			return
		}
		beforeServe.Store(&f)
	}
	// TableSessions lists the sessions of the runtime's table.
	TableSessions = func(r *Runtime) []*session.Established { return r.table.Sessions() }
	// ReServe forgets that the session is served and starts serving it again.
	ReServe = func(r *Runtime, est *session.Established) {
		r.serving.Delete(est.Session)
		r.ensureServing(est)
	}
	// LocalInfoOf returns the carrier-plan view built from the profile.
	LocalInfoOf = func(r *Runtime) carrier.LocalInfo { return r.planInfo() }
	// SessionLimitOf returns the limit of the running session table.
	SessionLimitOf = func(r *Runtime) int { return r.table.Max() }
	// NoteVisibility feeds one card visibility to the runtime (the card builder's Update hook).
	NoteVisibility = func(r *Runtime, v cards.Visibility) { r.noteVisibility(v) }
	// NoteListenerStopped reports a stopped accept loop.
	NoteListenerStopped = func(r *Runtime, err error) { r.listenerStopped(err) }
	// SetSecretObserver replaces secret.Observe for the memory-protection event test.
	SetSecretObserver = func(observe func(func(secret.Status))) { observeSecret = observe }
	// GreaseIntervalDefault exposes the default random interval.
	GreaseIntervalDefault func() time.Duration = defaultGreaseInterval
)

// HasHandler reports whether the runtime serves the service version (a registered module).
var HasHandler = func(r *Runtime, svc, version uint64) bool { return r.handlerFor(svc, version) != nil }
var RelaysForOthers = relaysForOthers

// SetDHTDialHook makes the DHT client of a runtime dial through the given options (memory carriers).
var SetDHTDialHook = func(r *Runtime, f func(*objects.Card) DialOpt) {
	r.dhtDialHook = f
	// The test networks live on private and loopback addresses.
	r.dhtClient.SetAllowNonRoutable(true)
}

// OwnDHTRecords returns what the runtime asks the DHT client to publish by itself.
var OwnDHTRecords = func(r *Runtime) [][]byte { return r.ownDHTRecords() }

// PeerCardOf returns the card the runtime uses for punching to peer: the card book first, then the
// card of a live session with that peer.
var PeerCardOf = func(r *Runtime, peer [32]byte) *objects.Card { return r.peerCard(DeviceID(peer)) }

// PunchRulesOf returns the target rules the runtime builds for a relayed session whose peer card is
// raw and whose PEER_ADDR is peer.
var PunchRulesOf = func(r *Runtime, raw []byte, peer netip.AddrPort) punch.Rules {
	ps := &punchState{peerAddr: peer, est: &session.Established{Peer: &handshake.CardInfo{Raw: raw}}}
	return r.relayc.rules(ps)
}

// CheckClockForTest runs the post-failure clock check (D36) as a failed dial would.
func (r *Runtime) CheckClockForTest() { r.maybeClockCheck() }

// AdmitVersionOf returns the handshake version admission of the runtime (T4.5).
var AdmitVersionOf = func(r *Runtime) func(uint32) bool { return r.admitVersion() }

// ReleaseCheckOf returns the release check handed to the DHT server.
var ReleaseCheckOf = func(r *Runtime) func(raw []byte, now time.Time) error { return r.releaseCheck() }

// RelayCardAddrDefault is the address rule F applies to relay cards.
var RelayCardAddrDefault = relayCardAddrDefault

// NextFetchPause is the pause of the mailbox fetch loop.
var NextFetchPause = nextFetchPause

// SetMailboxDialHook lets a test choose the dial options F uses to reach a mailbox relay (in-memory
// carriers).
var SetMailboxDialHook = func(r *Runtime, f func(*objects.Card) DialOpt) { r.mailboxDialHook = f }

// AllowRelayCardAddrs replaces the address check of relay cards; it returns the old check.
var AllowRelayCardAddrs = func(f func(netip.Addr) bool) func(netip.Addr) bool {
	old := relayCardAddrOK
	relayCardAddrOK = f
	return old
}

// MailboxReceiverTick runs one round of the node's mailbox receiver (the runtime also does it on a timer).
var MailboxReceiverTick = func(r *Runtime, ctx context.Context) error {
	if r.mailRecv == nil {
		return errors.New("no mailbox receiver")
	}
	return r.mailRecv.Tick(ctx)
}

// SetMailboxClientOpts lets a test choose the dial options of the node's mailbox client (in-memory carriers).
var SetMailboxClientOpts = func(r *Runtime, f func([32]byte) DialOpt) {
	r.mu.Lock()
	r.mailboxClientOpts = f
	r.mu.Unlock()
}

// MailboxQueues returns the current queues of the node's mailbox receiver.
var MailboxQueues = func(r *Runtime) []mailbox.QueueRef {
	if r.mailRecv == nil {
		return nil
	}
	return r.mailRecv.Queues()
}

// MailboxItems counts the items a mailbox relay holds (private queues).
var MailboxItems = func(r *Runtime) int { return r.mailboxItems() }

var (
	// NoteStall tells the runtime that a flow to ip stopped after bytes inbound bytes.
	NoteStall = func(r *Runtime, ip netip.Addr, bytes int) bool { return r.noteStall(ip, bytes) }
	// FrozenUntil is when the mark of the current network ends.
	FrozenUntil = func(r *Runtime) time.Time { return r.freeze.Until(r.NetworkID()) }
	// IsFrozen reports whether the runtime has marked its current network frozen.
	IsFrozen = func(r *Runtime) bool { return r.frozen() }
	// WrapFreeze wraps a connection to a foreign address the way the dial path does.
	WrapFreeze = func(r *Runtime, c net.Conn, ip netip.Addr) net.Conn { return r.freezeConn(c, ip) }
)

// NarrowPathOf returns the narrow-path marks of the runtime (T3.12).
var NarrowPathOf = func(r *Runtime) *carrier.NarrowPath { return r.narrow }

// Egress seams (T11.6a).
var (
	TCPOnly      = tcpOnly
	EgressDialer = func(e *Egress) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
		return e.dialer()
	}
	// PortMappingOf reports whether the effective profile still asks the router for ports.
	PortMappingOf = func(r *Runtime) bool { return r.cfg.Profile.PortMapping }
	// PortMapAdd exposes the manager the runtime built, to check it was built disabled.
	PortMapAdd = func(r *Runtime, proto portmap.Proto, port uint16) error { return r.pm.Add(proto, port) }
)
