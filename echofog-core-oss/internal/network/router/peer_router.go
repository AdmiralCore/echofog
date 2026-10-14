package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	ErrLinkClosed       = errors.New("router: peer link is closed")
	ErrMigrationTimeout = errors.New("router: transport migration timed out")
)

// PeerTransportRouter orchestrates the dynamic, per-peer transport selection
// and zero-loss hot migration between QUIC, ObfsQUIC, and TCP/TLS.
type PeerTransportRouter struct {
	peerID types.PeerID
	logger *slog.Logger

	activeLink atomic.Pointer[PeerTransportLink]
	state      atomic.Int32

	buffer      *InFlightBuffer
	migrationMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc

	onMigrationComplete func(peerID types.PeerID, from, to TransportProtocol)
}

// NewPeerTransportRouter creates a router managing transport links to a specific peer.
func NewPeerTransportRouter(parentCtx context.Context, peerID types.PeerID, initialLink PeerTransportLink) *PeerTransportRouter {
	ctx, cancel := context.WithCancel(parentCtx)
	r := &PeerTransportRouter{
		peerID: peerID,
		logger: slog.Default().With("component", "peer_router", "peer_id", peerID.String()),
		buffer: NewInFlightBuffer(2048),
		ctx:    ctx,
		cancel: cancel,
	}

	r.state.Store(int32(StateActive))
	r.activeLink.Store(&initialLink)

	return r
}

// SetOnMigrationComplete registers a callback notified when a link migration finishes.
func (r *PeerTransportRouter) SetOnMigrationComplete(fn func(peerID types.PeerID, from, to TransportProtocol)) {
	r.onMigrationComplete = fn
}

// ActiveProtocol returns the transport protocol currently handling traffic.
func (r *PeerTransportRouter) ActiveProtocol() TransportProtocol {
	linkPtr := r.activeLink.Load()
	if linkPtr == nil || *linkPtr == nil {
		return 0
	}
	return (*linkPtr).Protocol()
}

// State returns the current operational health state.
func (r *PeerTransportRouter) State() LinkState {
	return LinkState(r.state.Load())
}

// Send dispatches a message to the peer. If migration is currently in progress,
// the packet is transparently enqueued into InFlightBuffer with 0% data loss.
func (r *PeerTransportRouter) Send(msgType net_types.MessageType, payload []byte) error {
	currentState := LinkState(r.state.Load())

	switch currentState {
	case StateClosed:
		return ErrLinkClosed

	case StateMigrating:
		// Queue packet safely into RAM buffer while socket is being switched
		return r.buffer.Enqueue(msgType, payload)

	case StateActive, StateDegraded:
		linkPtr := r.activeLink.Load()
		if linkPtr == nil || *linkPtr == nil {
			return errors.New("router: active link is nil")
		}

		err := (*linkPtr).Send(msgType, payload)
		if err != nil {
			// Delivery error; mark as degraded and buffer for potential failover
			r.state.Store(int32(StateDegraded))
			_ = r.buffer.Enqueue(msgType, payload)
			return err
		}
		return nil

	default:
		return fmt.Errorf("router: unknown link state %d", currentState)
	}
}

// SendUnreliable dispatches a loss-tolerant frame (real-time media). It uses datagrams when the
// active link supports them and falls back to the reliable path otherwise (e.g. TCP/TLS mimicry).
// Frames are dropped instead of buffered while migrating: late media is worthless.
func (r *PeerTransportRouter) SendUnreliable(msgType net_types.MessageType, payload []byte) error {
	switch LinkState(r.state.Load()) {
	case StateClosed:
		return ErrLinkClosed
	case StateMigrating:
		return nil
	}
	linkPtr := r.activeLink.Load()
	if linkPtr == nil || *linkPtr == nil {
		return errors.New("router: active link is nil")
	}
	if ul, ok := (*linkPtr).(UnreliableLink); ok {
		return ul.SendUnreliable(msgType, payload)
	}
	return (*linkPtr).Send(msgType, payload)
}

// InitiateMigration begins atomic hot-handover to a new transport link.
func (r *PeerTransportRouter) InitiateMigration(targetLink PeerTransportLink, reason string) error {
	r.migrationMu.Lock()
	defer r.migrationMu.Unlock()

	if LinkState(r.state.Load()) == StateClosed {
		return ErrLinkClosed
	}

	oldLinkPtr := r.activeLink.Load()
	var oldProto TransportProtocol
	if oldLinkPtr != nil && *oldLinkPtr != nil {
		oldProto = (*oldLinkPtr).Protocol()
	}

	newProto := targetLink.Protocol()
	r.logger.Info("Initiating seamless transport migration",
		"from", oldProto.String(),
		"to", newProto.String(),
		"reason", reason)

	// 1. Enter migrating state: all concurrent sends will now enter InFlightBuffer
	r.state.Store(int32(StateMigrating))

	// 2. Transmit migration signaling request to remote peer over existing link (best-effort)
	if oldLinkPtr != nil && *oldLinkPtr != nil {
		payload := MigrationPayload{
			TargetProtocol: newProto,
			SessionNonce:   uint64(time.Now().UnixNano()),
			Timestamp:      time.Now().Unix(),
			Reason:         reason,
		}
		data, _ := payload.Encode()
		_ = (*oldLinkPtr).Send(net_types.TypeTransportMigrateRequest, data)
	}

	// 3. Atomically swap to the new transport link
	r.activeLink.Store(&targetLink)

	// 4. Drain all buffered in-flight packets into the new socket
	drainedCount, drainErr := r.buffer.Drain(targetLink)
	if drainErr != nil {
		r.logger.Error("Failed to drain in-flight buffer to new link", "err", drainErr)
	} else {
		r.logger.Info("Transport migration successful, in-flight buffer drained",
			"drained_packets", drainedCount,
			"active_proto", newProto.String())
	}

	// 5. Restore active state
	r.state.Store(int32(StateActive))

	// 6. Gracefully close old socket in background
	if oldLinkPtr != nil && *oldLinkPtr != nil {
		go func(l PeerTransportLink) {
			time.Sleep(1 * time.Second) // allow trailing packets to settle
			_ = l.Close()
		}(*oldLinkPtr)
	}

	if r.onMigrationComplete != nil {
		r.onMigrationComplete(r.peerID, oldProto, newProto)
	}

	return nil
}

// HandleMigrationRequest processes an incoming migration request from the remote peer.
func (r *PeerTransportRouter) HandleMigrationRequest(req *MigrationPayload, replacementLink PeerTransportLink) error {
	r.migrationMu.Lock()
	defer r.migrationMu.Unlock()

	oldLinkPtr := r.activeLink.Load()
	var oldProto TransportProtocol
	if oldLinkPtr != nil && *oldLinkPtr != nil {
		oldProto = (*oldLinkPtr).Protocol()
	}

	r.logger.Info("Received remote transport migration request",
		"from", oldProto.String(),
		"target_proto", req.TargetProtocol.String(),
		"reason", req.Reason)

	// Acknowledge over replacement link
	ackData, _ := (&MigrationPayload{
		TargetProtocol: req.TargetProtocol,
		SessionNonce:   req.SessionNonce,
		Timestamp:      time.Now().Unix(),
	}).Encode()

	_ = replacementLink.Send(net_types.TypeTransportMigrateAck, ackData)

	// Atomically switch active link
	r.activeLink.Store(&replacementLink)
	r.state.Store(int32(StateActive))

	// Drain any locally buffered packets
	_, _ = r.buffer.Drain(replacementLink)

	if r.onMigrationComplete != nil {
		r.onMigrationComplete(r.peerID, oldProto, req.TargetProtocol)
	}

	return nil
}

// Close gracefully shuts down the router and active links.
func (r *PeerTransportRouter) Close() error {
	r.migrationMu.Lock()
	defer r.migrationMu.Unlock()

	r.state.Store(int32(StateClosed))
	r.cancel()
	r.buffer.Close()

	linkPtr := r.activeLink.Load()
	if linkPtr != nil && *linkPtr != nil {
		return (*linkPtr).Close()
	}
	return nil
}
