package node

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/echofog/echofog-core/internal/v2/call"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/call/sigbridge"
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
	r.initCallTransport()
	late := &lateCallService{}
	bridge := sigbridge.New(sigbridge.Config{
		Self:    [32]byte(r.cfg.Device.ID()),
		Service: late,
		Guard:   call.NewJoinGuard(call.JoinGuardConfig{Now: r.clock.Now}),
		Deliver: r.callTr.DeliverSignal,
		Context: r.ctx,
		Now:     r.clock.Now,
	})
	srv, err := call.New(call.Config{
		Origin:    r.cfg.Device.Public(),
		Sign:      r.cfg.Device.Sign,
		Context:   r.ctx,
		Now:       r.clock.Now,
		Open:      r.openCallStream,
		Datagrams: &callDatagramPort{port: port},
		OnSignal:  bridge.OnSignal,
		Log:       r.log,
	})
	if err != nil {
		r.log.Warn("node: call signalling", "err", err)
		_ = port.Close()
		return
	}
	late.set(srv)
	r.callSvc = srv
	r.SetCallSignals(bridge)
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, callPeer(p), st)
	})
	r.startCallMedia()
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

// signalHolder lets the signal port be set after the runtime is built (the call layer is created
// by the app, once its identity and room exist).
type signalHolder struct{ port callnet.SignalPort }

// SetCallRoute sets network.call_route: the kind of path call media takes (T9.3). It applies to
// the next datagram sent or received.
func (r *Runtime) SetCallRoute(rt route.Route) {
	if r.egress != nil {
		rt = route.Relay // behind a proxy a direct media path would show the address (T11.6a)
	}
	r.callRoute.Store(uint32(rt))
}

// CallRoute returns the current call_route.
func (r *Runtime) CallRoute() route.Route { return route.Route(r.callRoute.Load()) }

// SetCallSignals sets the port that carries the call layer's signals (T9.1 call/2 mapping).
func (r *Runtime) SetCallSignals(p callnet.SignalPort) {
	r.callSignals.Store(&signalHolder{port: p})
}

// Call returns the call/2 signalling service (T9.1); nil when it is not registered.
func (r *Runtime) Call() *call.Service { return r.callSvc }

// CallTransport returns the call layer's transport over this node's sessions (T9.3). Nil when
// call/2 is not registered.
func (r *Runtime) CallTransport() *callnet.Transport { return r.callTr }

// ErrNoCallSignals: no signal port was set; call signals cannot be sent.
var ErrNoCallSignals = errors.New("node: no call signal port")

// callSignalPort forwards to the port set by SetCallSignals.
type callSignalPort struct{ r *Runtime }

func (p callSignalPort) SendSignal(to [32]byte, lossy bool, payload []byte) error {
	h := p.r.callSignals.Load()
	if h == nil || h.port == nil {
		return ErrNoCallSignals
	}
	return h.port.SendSignal(to, lossy, payload)
}

// callLinks gives the call transport the live session with a participant, without dialing: the
// call layer connects its tree neighbours itself.
type callLinks struct{ r *Runtime }

func (l callLinks) Link(device [32]byte) (route.Link, bool) {
	est := l.r.liveMainSession(DeviceID(device))
	if est == nil {
		return nil, false
	}
	return est.Session, true
}

// startCallMedia builds the call transport and pumps CALL_AUDIO / CALL_VIDEO datagrams into it.
func (r *Runtime) initCallTransport() {
	r.callTr = callnet.New(callnet.Config{
		Links:   callLinks{r},
		Signals: callSignalPort{r},
		Route:   r.CallRoute,
		Admit:   r.admitCallMedia,
		Now:     r.clock.Now,
	})
}

func (r *Runtime) startCallMedia() {
	port := r.Datagrams(frame.DgramCallAudio, frame.DgramCallVideo, frame.DgramCallAuth)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer port.Close()
		for {
			d, err := port.Receive(r.ctx)
			if err != nil {
				return
			}
			r.callTr.Deliver(callnet.Inbound{
				Device: [32]byte(d.Peer.Device), Type: d.Type, Payload: d.Payload, Path: d.PathKind,
			})
		}
	}()
}

// lateCallService lets the bridge be built before the service it sends through (the service's
// OnSignal is the bridge).
type lateCallService struct{ svc atomic.Pointer[call.Service] }

func (l *lateCallService) set(s *call.Service) { l.svc.Store(s) }

func (l *lateCallService) Send(ctx context.Context, p call.Peer, m *pb.CallSignal) error {
	s := l.svc.Load()
	if s == nil {
		return call.ErrClosed
	}
	return s.Send(ctx, p, m)
}

func (l *lateCallService) Forward(ctx context.Context, p call.Peer, raw []byte) error {
	s := l.svc.Load()
	if s == nil {
		return call.ErrClosed
	}
	return s.Forward(ctx, p, raw)
}

// SetCallAdmit sets who may send call media to this node: the members of the room it is in. The
// app updates it as the room changes. Until it is set, and for a nil function, no media is
// accepted: a node outside a call takes none (T9.3b).
func (r *Runtime) SetCallAdmit(f func(device [32]byte) bool) {
	if f == nil {
		r.callAdmit.Store(nil)
		return
	}
	r.callAdmit.Store(&f)
}

func (r *Runtime) admitCallMedia(device [32]byte) bool {
	f := r.callAdmit.Load()
	return f != nil && (*f)(device)
}
