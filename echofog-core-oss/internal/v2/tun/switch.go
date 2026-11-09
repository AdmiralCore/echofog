package tun

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// State of the kill switch.
type State uint8

const (
	// StateOff: no filters, no tunnel; the computer is on its own network.
	StateOff State = iota
	// StateActive: filters installed, tunnel up, DNS pinned.
	StateActive
	// StateHeld: the filters stay but the tunnel is gone (helper killed, adapter removed, an
	// earlier run crashed). Nothing leaves the computer. Reconnect or Stop leaves it.
	StateHeld
)

func (s State) String() string {
	switch s {
	case StateOff:
		return "off"
	case StateActive:
		return "active"
	case StateHeld:
		return "held"
	}
	return "unknown"
}

// ErrBadState is an operation that does not fit the current state.
var ErrBadState = errors.New("tun: operation does not fit the kill switch state")

// Timings. A rollback and a driver probe run on contexts of their own, so a cancelled caller
// cannot leave the computer half set up or a hung driver hold the Switch.
const (
	rollbackTimeout = 15 * time.Second
	aliveTimeout    = 2 * time.Second
	defaultPoll     = time.Second
)

// Config wires a Switch.
type Config struct {
	Firewall Firewall
	Tunnel   Tunnel
	DNS      DNSGuard
	Plan     Plan
	Tun      TunConfig
	// OnState is called after every state change, outside the locks, one call at a time and in the
	// order of the changes.
	OnState func(State, error)
	// PollInterval is how often the Switch asks Tunnel.Alive (0: once per second).
	PollInterval time.Duration
}

type change struct {
	state State
	err   error
}

// Switch is the fail-closed state machine of the whole-computer mode. The rule it keeps: the
// filters go in before the tunnel comes up, they stay when the tunnel is lost or the process dies,
// and they leave only when the user stops the mode and the firewall confirms it.
//
// Locks. opmu serializes the operations (Start, Reconnect, Update, Stop, Recover and the handling
// of a lost tunnel) and is held across driver calls. mu guards only the state and the queue of
// notifications and is never held across a driver call, so State() never waits for a driver.
// dmu serializes the delivery of notifications. Order: opmu, then mu; dmu is taken with neither.
type Switch struct {
	cfg    Config
	events <-chan TunEvent

	opmu sync.Mutex
	plan Plan // guarded by opmu

	mu      sync.Mutex
	state   State
	pending []change

	dmu sync.Mutex

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New returns a Switch in StateOff and starts watching the tunnel. Call Recover at start-up.
func New(cfg Config) *Switch {
	s := &Switch{cfg: cfg, plan: cfg.Plan, events: cfg.Tunnel.Events(), stop: make(chan struct{})}
	s.wg.Add(1)
	go s.watch()
	return s
}

// State returns the current state. It does not wait for a driver call.
func (s *Switch) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// set changes the state and queues the notification; flush delivers it. The caller holds opmu.
func (s *Switch) set(st State, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == st {
		return
	}
	s.state = st
	s.pending = append(s.pending, change{st, err})
}

// flush delivers the queued notifications. Deliveries are serialized by dmu and the queue is
// drained inside it, so they arrive in the order the changes were made, one at a time.
func (s *Switch) flush() {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	s.mu.Lock()
	p := s.pending
	s.pending = nil
	s.mu.Unlock()
	if s.cfg.OnState == nil {
		return
	}
	for _, c := range p {
		s.cfg.OnState(c.state, c.err)
	}
}

// watch turns a lost tunnel into StateHeld, from the driver's events and from a poll of the
// device. The filters are not touched: they are already persistent, and that is the point.
func (s *Switch) watch() {
	defer s.wg.Done()
	every := s.cfg.PollInterval
	if every <= 0 {
		every = defaultPoll
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	events := s.events
	for {
		select {
		case <-s.stop:
			return
		case ev, ok := <-events:
			if !ok {
				// A driver must not close the channel. Do not spin on it and do not read it as a loss:
				// the poll below still sees a dead tunnel.
				events = nil
				continue
			}
			s.lost(ev.Err)
		case <-tick.C:
			if s.State() != StateActive {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), aliveTimeout)
			alive := s.cfg.Tunnel.Alive(ctx)
			cancel()
			if !alive {
				s.lost(errors.New("tun: the tunnel is not answering"))
			}
		}
	}
}

// lost moves Active to Held. It waits for a running operation, so an event that arrives during
// Start is handled right after it; in any other state it is a stale event and is ignored.
func (s *Switch) lost(err error) {
	defer s.flush()
	s.opmu.Lock()
	defer s.opmu.Unlock()
	if s.State() != StateActive {
		return
	}
	if err == nil {
		err = errors.New("tun: the tunnel was lost")
	}
	s.set(StateHeld, err)
}

// Recover looks for filters an earlier process left and, if there are any, enters StateHeld
// without touching them: a crash never turns into a quiet leak.
func (s *Switch) Recover(ctx context.Context) error {
	defer s.flush()
	s.opmu.Lock()
	defer s.opmu.Unlock()
	if s.State() != StateOff {
		return ErrBadState
	}
	installed, err := s.cfg.Firewall.Installed(ctx)
	if err != nil {
		return fmt.Errorf("tun: look for kill switch filters: %w", err)
	}
	if installed {
		s.set(StateHeld, errors.New("tun: filters of an earlier run found"))
	}
	return nil
}

// Start installs the filters, brings the tunnel up and pins DNS, in that order. Any failure
// rolls back to StateOff, except that a failing rollback of the filters keeps StateHeld.
func (s *Switch) Start(ctx context.Context) error {
	defer s.flush()
	s.opmu.Lock()
	defer s.opmu.Unlock()
	if s.State() != StateOff {
		return ErrBadState
	}
	rules, err := BuildRules(s.plan)
	if err != nil {
		return err
	}
	// Without the right to install filters there is no tunnel: never VPN without the kill switch.
	if err := s.cfg.Firewall.Install(ctx, rules); err != nil {
		return err
	}
	if err := s.cfg.Tunnel.Up(ctx, s.cfg.Tun); err != nil {
		return s.rollback(ctx, err, false, false)
	}
	if err := s.cfg.DNS.Protect(ctx, s.dnsConfig()); err != nil {
		return s.rollback(ctx, err, true, true)
	}
	s.set(StateActive, nil)
	return nil
}

func (s *Switch) dnsConfig() DNSConfig {
	return DNSConfig{Tunnel: s.cfg.Tun.Name, Resolver: s.cfg.Tun.Resolver, DisableSmartNameResolution: true}
}

// rollback undoes a failed Start on a context of its own (the caller's may be the reason for the
// failure and is already cancelled). A half-applied DNS policy is restored too. If the filters
// cannot be removed the computer stays blocked (held) rather than open.
func (s *Switch) rollback(ctx context.Context, cause error, tunnelUp, dnsTried bool) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if dnsTried {
		_ = s.cfg.DNS.Restore(rctx)
	}
	if tunnelUp {
		_ = s.cfg.Tunnel.Down(rctx)
	}
	if err := s.cfg.Firewall.Remove(rctx); err != nil {
		s.set(StateHeld, err)
		return errors.Join(cause, fmt.Errorf("tun: the kill switch filters stay: %w", err))
	}
	return cause
}

// Reconnect brings the tunnel back up from StateHeld.
func (s *Switch) Reconnect(ctx context.Context) error {
	defer s.flush()
	s.opmu.Lock()
	defer s.opmu.Unlock()
	if s.State() != StateHeld {
		return ErrBadState
	}
	if err := s.cfg.Tunnel.Up(ctx, s.cfg.Tun); err != nil {
		return err
	}
	// The adapter is a new one: DNS is pinned again on it.
	if err := s.cfg.DNS.Protect(ctx, s.dnsConfig()); err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = s.cfg.Tunnel.Down(rctx)
		return err
	}
	s.set(StateActive, nil)
	return nil
}

// Update replaces the rules while the mode runs (the exit changed, the node moved, a key rotated).
// The new set goes in through Firewall.Install, which replaces the old one atomically; the filters
// are never removed, so there is no moment without them. In StateHeld the new plan is installed
// too (the next Reconnect then goes to the new endpoint). A plan that cannot be built, or an
// Install that fails, leaves the old rules and the old plan in place.
func (s *Switch) Update(ctx context.Context, p Plan) error {
	defer s.flush()
	s.opmu.Lock()
	defer s.opmu.Unlock()
	if st := s.State(); st != StateActive && st != StateHeld {
		return ErrBadState
	}
	rules, err := BuildRules(p)
	if err != nil {
		return err
	}
	if err := s.cfg.Firewall.Install(ctx, rules); err != nil {
		return err
	}
	s.plan = p
	return nil
}

// Stop is the user's explicit end: tunnel down, DNS restored, filters removed. If the filters
// cannot be removed the state stays held.
func (s *Switch) Stop(ctx context.Context) error {
	defer s.flush()
	s.opmu.Lock()
	defer s.opmu.Unlock()
	if s.State() == StateOff {
		return nil
	}
	var errs []error
	if err := s.cfg.Tunnel.Down(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.cfg.DNS.Restore(ctx); err != nil {
		// The policy still points at the resolver of a tunnel that is gone. Taking the filters away
		// now would hand back a computer that cannot resolve names, declared off. It stays held and
		// blocked; the next Stop tries again.
		s.set(StateHeld, err)
		errs = append(errs, fmt.Errorf("tun: DNS was not restored, the filters stay: %w", err))
		return errors.Join(errs...)
	}
	if err := s.cfg.Firewall.Remove(ctx); err != nil {
		s.set(StateHeld, err)
		errs = append(errs, fmt.Errorf("tun: the kill switch filters stay: %w", err))
		return errors.Join(errs...)
	}
	s.set(StateOff, nil)
	return errors.Join(errs...)
}

// SetTunnel replaces the configuration of the tunnel device for the next Start or Reconnect (the
// address of the node's proxy is known only once the VPN runs).
func (s *Switch) SetTunnel(tc TunConfig) {
	s.opmu.Lock()
	defer s.opmu.Unlock()
	s.cfg.Tun = tc
}

// Close ends the watcher; it does not change the filters.
func (s *Switch) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	return nil
}
