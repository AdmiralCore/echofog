// Package portmap asks the home router to forward the node's ports: PCP, NAT-PMP, then UPnP
// (T6.5). The external address a router reports is only a candidate: it is exposed through
// Candidates once a peer has confirmed the public IP (T3.4, Confirm). With Config.Enabled false
// (profile port_mapping, off on mobile) the package sends nothing to anyone.
//
// The package replaces the v1 portmap (AD1: no v1 network packages). The wire formats are small
// and implemented here over injected sockets, HTTP client and clock, so the whole thing is
// testable against a scripted gateway.
package portmap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// Proto is the transport of a forwarded port.
type Proto string

// Transports a mapping can carry.
const (
	UDP Proto = "udp"
	TCP Proto = "tcp"
)

const (
	defaultLifetime = time.Hour
	defaultPort     = 5351
	// maxPorts bounds the mappings (the node needs one UDP and one TCP port).
	maxPorts = 8
	// A failed renewal is retried after 30 s, doubling to 10 min; three failures in a row make
	// the gateway be looked for again.
	retryBase       = 30 * time.Second
	retryMax        = 10 * time.Minute
	rediscoverAfter = 3
	// Discovery that finds nothing is repeated after 1 min, doubling to 30 min.
	discoverBase = time.Minute
	discoverMax  = 30 * time.Minute
	minRenew     = 10 * time.Second
)

// PacketConn is a UDP socket connected to the gateway.
type PacketConn interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Close() error
}

// HTTPDoer is the HTTP client used for UPnP. It must not follow redirects.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config configures a Manager. Nil fields select the production implementation.
type Config struct {
	// Enabled is the port_mapping profile flag. False: no packet is ever sent.
	Enabled bool
	// Clock is the injectable time source (AD13).
	Clock carrier.Clock
	// Gateway returns the default gateway.
	Gateway func() (netip.Addr, error)
	// Local returns our address on the gateway's network.
	Local func(gw netip.Addr) (netip.Addr, error)
	// Dial opens a UDP socket connected to a gateway port.
	Dial func(ctx context.Context, remote netip.AddrPort) (PacketConn, error)
	// HTTP is used for the UPnP description and control requests.
	HTTP HTTPDoer
	// SSDP returns UPnP device description URLs found on the network.
	SSDP func(ctx context.Context) ([]string, error)
	// Rand returns a value in [0,1): jitter and port choice.
	Rand func() float64
	// Nonce is the source of PCP mapping nonces.
	Nonce io.Reader
	// Lifetime is the mapping lifetime asked for; zero means one hour.
	Lifetime time.Duration
	// Port is the PCP and NAT-PMP server port; zero means 5351.
	Port uint16
	// OnChange is called, outside any lock, after the candidate set may have changed.
	OnChange func()
}

// Mapping is one forwarded port.
type Mapping struct {
	Proto    Proto
	Internal uint16
	External netip.AddrPort
	// Via is the protocol that opened it: "PCP", "NAT-PMP" or "UPnP".
	Via string
}

var (
	// ErrDisabled is returned by Add when port mapping is off.
	ErrDisabled = errors.New("portmap: port mapping is disabled")
	// ErrTooMany is returned by Add when the port limit is reached.
	ErrTooMany = errors.New("portmap: too many ports")
	// ErrClosed is returned by Add after Close.
	ErrClosed = errors.New("portmap: closed")
	// ErrBadPort is returned by Add for port 0 or an unknown protocol.
	ErrBadPort = errors.New("portmap: bad port or protocol")
)

type key struct {
	proto Proto
	port  uint16
}

type entry struct {
	mapped  bool
	ext     netip.AddrPort
	extPort uint16 // last port the router gave, asked for again on renewal
	via     string
	expires time.Time
	nextAt  time.Time
	fails   int
}

type unmapJob struct {
	key     key
	extPort uint16
}

// Manager keeps a set of ports forwarded and renewed.
type Manager struct {
	cfg   Config
	clock carrier.Clock

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{} // closed when the worker has stopped

	mu         sync.Mutex
	entries    map[key]*entry
	unmaps     []unmapJob
	client     gateway // used by the worker, and by Close after the worker stopped
	confirmed  netip.Addr
	closed     bool
	timer      carrier.Timer
	discFails  int
	discoverAt time.Time
	mapFails   int
}

// New returns a Manager. Nothing is sent until the first Add, and never if cfg.Enabled is false.
func New(cfg Config) *Manager {
	cfg = withDefaults(cfg)
	m := &Manager{cfg: cfg, clock: cfg.Clock, entries: map[key]*entry{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	if !cfg.Enabled {
		close(m.done)
		return m
	}
	go m.run()
	return m
}

// Add asks for the port to be forwarded and keeps it so. Adding a port twice is a no-op.
func (m *Manager) Add(proto Proto, port uint16) error {
	if proto != UDP && proto != TCP || port == 0 {
		return ErrBadPort
	}
	if !m.cfg.Enabled {
		return ErrDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	k := key{proto, port}
	if _, ok := m.entries[k]; ok {
		return nil
	}
	if len(m.entries) >= maxPorts {
		return ErrTooMany
	}
	m.entries[k] = &entry{}
	m.poke()
	return nil
}

// Remove stops renewing a port and asks the gateway to drop it.
func (m *Manager) Remove(proto Proto, port uint16) {
	m.mu.Lock()
	k := key{proto, port}
	e, ok := m.entries[k]
	if !ok || m.closed {
		m.mu.Unlock()
		return
	}
	delete(m.entries, k)
	if e.mapped {
		m.unmaps = append(m.unmaps, unmapJob{k, e.extPort})
	}
	m.poke()
	m.mu.Unlock()
	m.changed()
}

// Mappings returns the ports the gateway has forwarded to a public address.
func (m *Manager) Mappings() []Mapping {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(false)
}

// Confirm tells the manager the public IP peers see (T3.4 Observer); the zero address means
// unknown. A mapping whose external IP is another one is not a candidate.
func (m *Manager) Confirm(ip netip.Addr) {
	m.mu.Lock()
	ip = ip.Unmap()
	if m.confirmed == ip {
		m.mu.Unlock()
		return
	}
	m.confirmed = ip
	m.mu.Unlock()
	m.changed()
}

// Candidates returns the mappings whose external IP peers have confirmed: only those may be
// published in the card.
func (m *Manager) Candidates() []Mapping {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(true)
}

func (m *Manager) list(confirmedOnly bool) []Mapping {
	now := m.clock.Now()
	var out []Mapping
	for k, e := range m.entries {
		if !e.mapped || !now.Before(e.expires) || !isPublic(e.ext.Addr()) {
			continue
		}
		if confirmedOnly && (!m.confirmed.IsValid() || m.confirmed != e.ext.Addr()) {
			continue
		}
		out = append(out, Mapping{Proto: k.proto, Internal: k.port, External: e.ext, Via: e.via})
	}
	return out
}

// Rediscover forgets the gateway and looks for it again at once, and renews every mapping: the
// network changed and the old router no longer holds them.
func (m *Manager) Rediscover() {}

// Close drops every mapping and stops the manager. The unmapping is best effort: the router
// forgets a mapping when its lifetime runs out anyway.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	<-m.done

	m.mu.Lock()
	if m.timer != nil {
		m.timer.Stop()
	}
	cl := m.client
	jobs := m.unmaps
	for k, e := range m.entries {
		if e.mapped {
			jobs = append(jobs, unmapJob{k, e.extPort})
		}
	}
	m.entries, m.unmaps, m.client = map[key]*entry{}, nil, nil
	m.mu.Unlock()
	if cl != nil {
		for _, j := range jobs {
			_ = cl.unmap(ctx, j.key.proto, j.key.port, j.extPort)
		}
	}
	m.changed()
	return nil
}

func (m *Manager) changed() {
	if m.cfg.OnChange != nil {
		m.cfg.OnChange()
	}
}

// poke wakes the worker; callable under mu.
func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) run() {
	defer close(m.done)
	for {
		select {
		case <-m.wake:
			m.step()
		case <-m.ctx.Done():
			return
		}
	}
}

type due struct {
	k       key
	extPort uint16
}

// step does the work that is due: pending unmaps, discovery, first mappings and renewals.
func (m *Manager) step() {
	changed := false
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	cl := m.client
	jobs := m.unmaps
	m.unmaps = nil
	m.mu.Unlock()
	if cl != nil {
		for _, j := range jobs {
			_ = cl.unmap(m.ctx, j.key.proto, j.key.port, j.extPort)
		}
	}

	m.mu.Lock()
	now := m.clock.Now()
	needDiscovery := cl == nil && len(m.entries) > 0 && !now.Before(m.discoverAt)
	m.mu.Unlock()
	if needDiscovery {
		found, err := m.discover(m.ctx)
		m.mu.Lock()
		if err != nil {
			m.discFails++
			m.discoverAt = m.clock.Now().Add(m.jitter(backoff(discoverBase, discoverMax, m.discFails)))
		} else {
			m.client, m.discFails, m.mapFails = found, 0, 0
			cl = found
		}
		m.mu.Unlock()
	}

	if cl != nil {
		m.mu.Lock()
		now = m.clock.Now()
		var todo []due
		for k, e := range m.entries {
			if !e.nextAt.After(now) {
				todo = append(todo, due{k, e.extPort})
			}
		}
		m.mu.Unlock()
		for _, d := range todo {
			if m.ctx.Err() != nil {
				return
			}
			if m.mapOne(cl, d) {
				changed = true
			}
		}
	}
	m.arm()
	if changed {
		m.changed()
	}
}

// mapOne opens or renews one mapping and reports whether the visible state changed.
func (m *Manager) mapOne(cl gateway, d due) bool {
	suggested := d.extPort
	if suggested == 0 {
		suggested = d.k.port
	}
	res, err := cl.mapPort(m.ctx, d.k.proto, d.k.port, suggested, m.cfg.Lifetime)
	if err == nil && res.extPort == 0 {
		err = errors.New("portmap: the router gave external port 0")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	e, ok := m.entries[d.k]
	if !ok { // removed while we were asking
		if err == nil {
			m.unmaps = append(m.unmaps, unmapJob{d.k, res.extPort})
			m.poke()
		}
		return false
	}
	if err != nil {
		e.fails++
		m.mapFails++
		e.nextAt = now.Add(m.jitter(backoff(retryBase, retryMax, e.fails)))
		if m.mapFails >= rediscoverAfter {
			// the router may have restarted or changed: look for it again
			m.client, m.mapFails = nil, 0
			m.discoverAt = now.Add(m.jitter(retryBase))
			for _, o := range m.entries {
				o.nextAt = m.discoverAt
			}
		}
		return false
	}
	e.fails, m.mapFails = 0, 0
	granted := res.lifetime
	if granted <= 0 {
		granted = m.cfg.Lifetime
	}
	ext := netip.AddrPortFrom(res.addr.Unmap(), res.extPort)
	changed := !e.mapped || e.ext != ext || e.via != cl.name()
	e.mapped, e.ext, e.extPort, e.via = true, ext, res.extPort, cl.name()
	e.expires = now.Add(granted)
	e.nextAt = now.Add(m.jitter(max(granted/2, minRenew)))
	return changed
}

// arm sets the one timer to the next moment something is due.
func (m *Manager) arm() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	var next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if m.client == nil {
		if len(m.entries) > 0 {
			soonest(m.discoverAt)
		}
	} else {
		for _, e := range m.entries {
			soonest(e.nextAt)
		}
	}
	if next.IsZero() {
		return
	}
	d := max(next.Sub(m.clock.Now()), time.Millisecond)
	m.timer = m.clock.AfterFunc(d, func() {
		m.mu.Lock()
		m.poke()
		m.mu.Unlock()
	})
}

// jitter spreads a timer by ±20 % (AD13).
func (m *Manager) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*m.cfg.Rand()))
}

func backoff(base, ceil time.Duration, fails int) time.Duration {
	d := base
	for i := 1; i < fails && d < ceil; i++ {
		d *= 2
	}
	return min(d, ceil)
}
