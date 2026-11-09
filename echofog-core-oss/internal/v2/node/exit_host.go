package node

import (
	"context"
	"errors"
	"net/netip"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// ExitOptions is how a node that is not an exit by profile (a desktop) offers one for a while (T10.9).
type ExitOptions struct {
	// Admit admits a client by the identity it uses for this exit (never the messenger identity) and
	// the address it came from; nil admits everybody. It is asked for every new stream and again by
	// RecheckExit, and must have no side effects.
	Admit func(node [32]byte, remote netip.AddrPort) bool
	// Refused is told about every client Admit turned away.
	Refused func(node [32]byte, remote netip.AddrPort)
	// MaxMbps limits what one client may take, in megabits a second; 0 keeps the default.
	MaxMbps int
	// AllowPrivateTargets lets clients reach private addresses: tests and closed deployments only.
	AllowPrivateTargets bool
}

// ExitClient is a client with streams open.
type ExitClient struct {
	Node    [32]byte
	Streams int
}

// ExitStatus is what the hosted exit has done.
type ExitStatus struct {
	Enabled           bool
	Accepted, Refused uint64
	Bytes             uint64
	RefusedBy         map[string]uint64
	Clients           []ExitClient
}

// Errors of the hosted exit.
var (
	// ErrExitStatic reports an exit that the profile already carries: it cannot be switched.
	ErrExitStatic = errors.New("node: the exit role is part of the profile")
	// ErrExitRunning reports EnableExit on an exit that is on.
	ErrExitRunning = errors.New("node: the exit is already on")
)

// hostedOffer is the live exit offer of a desktop: free, no payment (payment and the market come
// after 0.3.0).
type hostedOffer struct{}

func (hostedOffer) ExitOffer() *pb.ExitOffer { return &pb.ExitOffer{} }

type hostedExit struct {
	srv    *exit.Server
	cancel context.CancelFunc
	done   chan struct{}
}

// EnableExit starts exit/2 and puts the exit offer on the cards, without a restart. Sessions that are
// already open did not negotiate the service; the next one does.
func (r *Runtime) EnableExit(opt ExitOptions) error {
	if r.cfg.Profile.HasRole(RoleExit) {
		return ErrExitStatic
	}
	spec, ok := service.Builtin(service.Exit)
	if !ok {
		return errors.New("node: exit/2 is not built in")
	}
	r.hostMu.Lock()
	defer r.hostMu.Unlock()
	if r.hosted != nil {
		return ErrExitRunning
	}
	cs := core.NewExitService()
	if opt.MaxMbps > 0 {
		l := cs.CurrentLimits()
		l.FreePeerRate = float64(opt.MaxMbps) * 1e6 / 8
		cs.SetLimits(l)
	}
	srv, err := exit.NewServer(exit.ServerConfig{
		Core:                cs,
		Clock:               r.carrierClock,
		Shape:               exitShape(r.cfg),
		SelfIPs:             r.exitSelfIPs,
		AllowPrivateTargets: opt.AllowPrivateTargets,
	})
	if err != nil {
		return err
	}
	if opt.Admit != nil {
		srv.SetAccess(func(p exit.Peer) bool { return opt.Admit(p.Node, p.Remote) }, func(p exit.Peer) {
			if opt.Refused != nil {
				opt.Refused(p.Node, p.Remote)
			}
		})
	}
	ctx, cancel := context.WithCancel(r.ctx)
	port := r.Datagrams(frame.DgramExitUDP)
	h := &hostedExit{srv: srv, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		defer port.Close()
		for {
			d, err := port.Receive(ctx)
			if err != nil {
				return
			}
			srv.HandleDatagram(r.exitPeer(d.Peer), d.Payload)
		}
	}()
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, r.exitPeer(p), st)
	})
	if err := r.cfg.Cards.SetExit(hostedOffer{}); err != nil {
		r.services.Remove(service.Exit)
		cancel()
		<-h.done
		return err
	}
	r.hosted = h
	return nil
}

// DisableExit takes the offer off the cards, stops exit/2, refuses new streams and closes the live ones.
func (r *Runtime) DisableExit() {
	r.hostMu.Lock()
	h := r.hosted
	r.hosted = nil
	r.hostMu.Unlock()
	if h == nil {
		return
	}
	_ = r.cfg.Cards.SetExit(nil)
	r.services.Remove(service.Exit)
	r.mu.Lock()
	for k := range r.handlers {
		if k[0] == service.Exit {
			delete(r.handlers, k)
		}
	}
	r.mu.Unlock()
	h.srv.SetEnabled(false)
	h.cancel()
	<-h.done
}

// RecheckExit closes the open streams of the clients that are no longer admitted.
func (r *Runtime) RecheckExit() {
	r.hostMu.Lock()
	h := r.hosted
	r.hostMu.Unlock()
	if h != nil {
		h.srv.Recheck()
	}
}

// ExitStatus reports the hosted exit; the zero value when it is off.
func (r *Runtime) ExitStatus() ExitStatus {
	r.hostMu.Lock()
	h := r.hosted
	r.hostMu.Unlock()
	if h == nil {
		return ExitStatus{}
	}
	st := ExitStatus{Enabled: true}
	st.Accepted, st.Refused, st.Bytes = h.srv.Totals()
	st.RefusedBy = h.srv.RefusedBy()
	for _, c := range h.srv.Live() {
		st.Clients = append(st.Clients, ExitClient{Node: c.Node, Streams: c.Streams})
	}
	return st
}
