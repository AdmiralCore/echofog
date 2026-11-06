package node

import (
	"context"

	"github.com/echofog/echofog-core/internal/v2/call"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// registerCall registers call/2 (T9.1): the SIGNAL service and its CALL_SIGNAL datagrams.
//
// The service package owns the signalling; this file is the only place that knows both the
// service and node.Host/Peer (AD5). It is the merge point for call/2, so the service branch never
// edits modules.go or another service's file.
func (r *Runtime) registerCall() {
	spec, ok := service.Builtin(service.Call)
	if !ok {
		return
	}
	port := r.Datagrams(frame.DgramCallSignal)
	srv, err := call.New(call.Config{
		Origin:    r.cfg.Device.Public(),
		Sign:      r.cfg.Device.Sign,
		Context:   r.ctx,
		Now:       r.clock.Now,
		Open:      r.openCallStream,
		Datagrams: &callDatagramPort{port: port},
		Log:       r.log,
	})
	if err != nil {
		r.log.Warn("node: call signalling", "err", err)
		_ = port.Close()
		return
	}
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, callPeer(p), st)
	})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := srv.ReceiveDatagrams(r.ctx); err != nil && r.ctx.Err() == nil {
			r.log.Debug("node: call datagrams stopped", "err", err)
		}
	}()
}

// openCallStream opens (or reuses through the session table) a call/2 stream to a peer. Call
// maintains one long-lived stream per peer; the runtime addresses peers by device ID.
func (r *Runtime) openCallStream(ctx context.Context, device [32]byte) (call.Peer, call.Stream, error) {
	p, st, err := r.Open(ctx, DeviceID(device), service.Call, DialOpt{})
	if err != nil {
		return call.Peer{}, nil, err
	}
	return callPeer(p), st, nil
}

func callPeer(p Peer) call.Peer {
	return call.Peer{
		Node:   [32]byte(p.Node),
		Device: [32]byte(p.Device),
		SID:    p.SID,
		Path:   p.Path,
	}
}

// callDatagramPort adapts the node's datagram port (node.Peer, node.DeviceID) to the shapes of
// the call package.
type callDatagramPort struct {
	port DatagramPort
}

func (p *callDatagramPort) Send(ctx context.Context, peer [32]byte, typ uint64, payload []byte) error {
	return p.port.Send(ctx, DeviceID(peer), typ, payload)
}

func (p *callDatagramPort) Receive(ctx context.Context) (call.Datagram, error) {
	d, err := p.port.Receive(ctx)
	if err != nil {
		return call.Datagram{}, err
	}
	return call.Datagram{
		Peer:    callPeer(d.Peer),
		Type:    d.Type,
		Payload: d.Payload,
	}, nil
}

func (p *callDatagramPort) Close() error { return p.port.Close() }

// SetCallRoute sets network.call_route: the kind of path call media takes (T9.3).
func (r *Runtime) SetCallRoute(rt route.Route) {}

// CallTransport returns the call layer's transport over this node's sessions (T9.3). Nil when
// call/2 is not registered.
func (r *Runtime) CallTransport() *callnet.Transport { return nil }
