package node

import (
	"context"
	"net"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// registerExit registers exit/2 (T10.1) when the profile carries the exit role.
//
// The service package owns the handler and calls r.Register with the built-in spec. This file is
// the merge point for exit/2: the service branch never edits modules.go or another service's file.
func (r *Runtime) registerExit() {
	if !r.cfg.Profile.HasRole(RoleExit) {
		return
	}
	spec, ok := service.Builtin(service.Exit)
	if !ok {
		return
	}
	srv, err := exit.NewServer(exit.ServerConfig{
		Clock:   r.carrierClock,
		Shape:   exitShape(r.cfg),
		SelfIPs: r.exitSelfIPs,
	})
	if err != nil {
		r.log.Error("exit/2 disabled", "err", err)
		return
	}
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, r.exitPeer(p), st)
	})
	port := r.Datagrams(frame.DgramExitUDP)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer port.Close()
		for {
			d, err := port.Receive(r.ctx)
			if err != nil {
				return
			}
			srv.HandleDatagram(r.exitPeer(d.Peer), d.Payload)
		}
	}()
}

// exitPeer describes the authenticated session of a client; nothing in it comes from a frame.
func (r *Runtime) exitPeer(p Peer) exit.Peer {
	return exit.Peer{
		Node:         [32]byte(p.Node),
		Device:       [32]byte(p.Device),
		SID:          p.SID,
		Remote:       p.Remote,
		SendDatagram: r.sendDatagramTo(p.SID),
	}
}

// exitSelfIPs reports this exit's public and local addresses: a client that reached one of them
// would come back through the operator's router or LAN (services.md §7).
func (r *Runtime) exitSelfIPs() []net.IP {
	var out []net.IP
	for _, a := range r.cfg.LocalInfo.Addrs {
		out = append(out, net.IP(a.AsSlice()))
	}
	r.localMu.RLock()
	for _, a := range r.localAddrs {
		out = append(out, net.IP(a.AsSlice()))
	}
	r.localMu.RUnlock()
	return out
}
