package tun

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// Config wires a Switch.
type Config struct {
	Firewall Firewall
	Tunnel   Tunnel
	DNS      DNSGuard
	Plan     Plan
	Tun      TunConfig
	// OnState is called after every state change, outside the lock.
	OnState func(State, error)
}

type change struct {
	state State
	err   error
}

// Switch is the fail-closed state machine of the whole-computer mode. The rule it keeps: the
// filters go in before the tunnel comes up, they stay when the tunnel is lost or the process dies,
// and they leave only when the user stops the mode and the firewall confirms it.
type Switch struct {
	cfg    Config
	events <-chan TunEvent

	mu      sync.Mutex
	state   State
	pending []change

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New returns a Switch in StateOff and starts watching the tunnel. Call Recover at start-up.
func New(cfg Config) *Switch {
	s := &Switch{cfg: cfg, events: cfg.Tunnel.Events(), stop: make(chan struct{})}
	s.wg.Add(1)
	go s.watch()
	return s
}

// State returns the current state.
func (s *Switch) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// set changes the state under mu and queues the notification; flush delivers it after unlock.
func (s *Switch) set(st State, err error) {
	if s.state == st {
		return
	}
	s.state = st
	s.pending = append(s.pending, change{st, err})
}

func (s *Switch) flush() {
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

// watch turns a lost tunnel into StateHeld. The filters are not touched: they are already
// persistent, and that is the point.
func (s *Switch) watch() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case ev := <-s.events:
			s.mu.Lock()
			if s.state == StateActive {
				err := ev.Err
				if err == nil {
					err = errors.New("tun: the tunnel was lost")
				}
				s.set(StateHeld, err)
			}
			s.mu.Unlock()
			s.flush()
		}
	}
}

// Recover looks for filters an earlier process left and, if there are any, enters StateHeld
// without touching them: a crash never turns into a quiet leak.
func (s *Switch) Recover(ctx context.Context) error {
	defer s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateOff {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateOff {
		return ErrBadState
	}
	rules, err := BuildRules(s.cfg.Plan)
	if err != nil {
		return err
	}
	// Without the right to install filters there is no tunnel: never VPN without the kill switch.
	if err := s.cfg.Firewall.Install(ctx, rules); err != nil {
		return err
	}
	if err := s.cfg.Tunnel.Up(ctx, s.cfg.Tun); err != nil {
		return s.rollback(ctx, err, false)
	}
	if err := s.cfg.DNS.Protect(ctx, s.dnsConfig()); err != nil {
		return s.rollback(ctx, err, true)
	}
	s.set(StateActive, nil)
	return nil
}

func (s *Switch) dnsConfig() DNSConfig {
	return DNSConfig{Tunnel: s.cfg.Tun.Name, Resolver: s.cfg.Tun.Resolver, DisableSmartNameResolution: true}
}

// rollback undoes a failed Start. If the filters cannot be removed the computer stays blocked
// (held) rather than open.
func (s *Switch) rollback(ctx context.Context, cause error, tunnelUp bool) error {
	if tunnelUp {
		_ = s.cfg.Tunnel.Down(ctx)
	}
	if err := s.cfg.Firewall.Remove(ctx); err != nil {
		s.set(StateHeld, err)
		return errors.Join(cause, fmt.Errorf("tun: the kill switch filters stay: %w", err))
	}
	return cause
}

// Reconnect brings the tunnel back up from StateHeld.
func (s *Switch) Reconnect(ctx context.Context) error {
	defer s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateHeld {
		return ErrBadState
	}
	if err := s.cfg.Tunnel.Up(ctx, s.cfg.Tun); err != nil {
		return err
	}
	// The adapter is a new one: DNS is pinned again on it.
	if err := s.cfg.DNS.Protect(ctx, s.dnsConfig()); err != nil {
		_ = s.cfg.Tunnel.Down(ctx)
		return err
	}
	s.set(StateActive, nil)
	return nil
}

// Stop is the user's explicit end: tunnel down, DNS restored, filters removed. If the filters
// cannot be removed the state stays held.
func (s *Switch) Stop(ctx context.Context) error {
	defer s.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateOff {
		return nil
	}
	var errs []error
	if err := s.cfg.Tunnel.Down(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.cfg.DNS.Restore(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.cfg.Firewall.Remove(ctx); err != nil {
		s.set(StateHeld, err)
		errs = append(errs, fmt.Errorf("tun: the kill switch filters stay: %w", err))
		return errors.Join(errs...)
	}
	s.set(StateOff, nil)
	return errors.Join(errs...)
}

// Close ends the watcher; it does not change the filters.
func (s *Switch) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	return nil
}
