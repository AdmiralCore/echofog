package portmap

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

// gateway is one way of talking to the router once it has been found.
type gateway interface {
	name() string
	// mapPort opens or renews a mapping, asking for the external port suggested.
	mapPort(ctx context.Context, proto Proto, internal, suggested uint16, lifetime time.Duration) (mapResult, error)
	// unmap drops a mapping; best effort, it does not wait for an answer.
	unmap(ctx context.Context, proto Proto, internal, ext uint16) error
}

type mapResult struct {
	extPort  uint16
	addr     netip.Addr
	lifetime time.Duration // what the router granted; zero if it did not say
}

var (
	errNoAnswer    = errors.New("portmap: no answer from the gateway")
	errUnsupported = errors.New("portmap: protocol not supported by the gateway")
	errNoGateway   = errors.New("portmap: no gateway on the local network")
)

// discover finds the way to the router: PCP, then NAT-PMP, then UPnP. Each is tried once, the
// first that answers wins.
func (m *Manager) discover(ctx context.Context) (gateway, error) {
	gw, err := m.cfg.Gateway()
	if err != nil {
		return nil, err
	}
	gw = gw.Unmap()
	if !isLANGateway(gw) {
		return nil, errNoGateway
	}
	local, err := m.cfg.Local(gw)
	if err != nil {
		return nil, err
	}
	if g, err := m.discoverPCP(ctx, gw, local); err == nil {
		return g, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if g, err := m.discoverNATPMP(ctx, gw); err == nil {
		return g, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return m.discoverUPnP(ctx, gw, local)
}

// withTimeout bounds ctx with a deadline on the injected clock (AD13).
func (m *Manager) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	tm := m.clock.AfterFunc(d, cancel)
	return ctx, func() { tm.Stop(); cancel() }
}

// exchangeWaits is the retransmission schedule of NAT-PMP (RFC 6886 starts at 250 ms) cut to
// three tries: a start-up check must not take half a minute.
var exchangeWaits = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

const maxPacket = 1100 // the largest PCP message

// exchange sends req and feeds the answers to accept until it reports done. accept ignores
// packets that are not the answer by returning done=false. The socket is connected to the
// gateway, so the system already drops packets from anyone else.
func (m *Manager) exchange(ctx context.Context, server netip.AddrPort, req []byte, accept func(p []byte) (done bool, err error)) error {
	conn, err := m.cfg.Dial(ctx, server)
	if err != nil {
		return err
	}
	defer conn.Close()
	pkts := make(chan []byte, 8)
	go func() {
		buf := make([]byte, maxPacket+1)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			select {
			case pkts <- append([]byte(nil), buf[:n]...):
			default: // a flood is dropped, not queued
			}
		}
	}()
	for _, wait := range exchangeWaits {
		if _, err := conn.Write(req); err != nil {
			return err
		}
		tick := make(chan struct{}, 1)
		tm := m.clock.AfterFunc(wait, func() { tick <- struct{}{} })
	wait:
		for {
			select {
			case p := <-pkts:
				if len(p) > maxPacket {
					continue
				}
				if done, err := accept(p); done {
					tm.Stop()
					return err
				}
			case <-tick:
				break wait
			case <-ctx.Done():
				tm.Stop()
				return ctx.Err()
			}
		}
	}
	return errNoAnswer
}

// send writes one datagram without waiting for an answer.
func (m *Manager) send(ctx context.Context, server netip.AddrPort, req []byte) error {
	conn, err := m.cfg.Dial(ctx, server)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(req)
	return err
}
