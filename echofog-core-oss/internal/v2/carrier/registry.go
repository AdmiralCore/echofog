package carrier

import (
	"sync"
)

// Registry maps L0 suite numbers (registries.md §6.1) to carrier implementations. Suite packages
// build their configured dialer or listener and register it during startup; the carrier package
// stays free of imports from them.
//
// Registration is a startup action: it panics on a nil implementation, suite 0 or a duplicate,
// because those are programming errors, not runtime conditions. Lookups are safe for concurrent
// use with registration.
type Registry struct {
	mu        sync.RWMutex
	dialers   map[uint32]Dialer
	listeners map[uint32]Listener
}

// NewRegistry returns an empty registry. The package-level Register* functions use
// defaultRegistry; tests and embedding applications may use their own.
func NewRegistry() *Registry {
	return &Registry{
		dialers:   make(map[uint32]Dialer),
		listeners: make(map[uint32]Listener),
	}
}

var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry used by the package-level functions.
func DefaultRegistry() *Registry { return defaultRegistry }

// RegisterDialer registers d for suite. It panics on a nil dialer, suite 0 or a duplicate.
func (r *Registry) RegisterDialer(suite uint32, d Dialer) {
	if suite == 0 {
		panic("carrier: dialer for reserved suite 0")
	}
	if d == nil {
		panic("carrier: nil dialer")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.dialers[suite]; ok {
		panic("carrier: duplicate dialer registration")
	}
	r.dialers[suite] = d
}

// RegisterListener registers l for suite. It panics on a nil listener, suite 0 or a duplicate.
func (r *Registry) RegisterListener(suite uint32, l Listener) {
	if suite == 0 {
		panic("carrier: listener for reserved suite 0")
	}
	if l == nil {
		panic("carrier: nil listener")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.listeners[suite]; ok {
		panic("carrier: duplicate listener registration")
	}
	r.listeners[suite] = l
}

// DialerFor returns the dialer registered for suite.
func (r *Registry) DialerFor(suite uint32) (Dialer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.dialers[suite]
	return d, ok
}

// ListenerFor returns the listener registered for suite.
func (r *Registry) ListenerFor(suite uint32) (Listener, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.listeners[suite]
	return l, ok
}

// RegisterDialer registers d in the default registry (see Registry.RegisterDialer).
func RegisterDialer(suite uint32, d Dialer) { defaultRegistry.RegisterDialer(suite, d) }

// RegisterListener registers l in the default registry (see Registry.RegisterListener).
func RegisterListener(suite uint32, l Listener) { defaultRegistry.RegisterListener(suite, l) }

// DialerFor returns the dialer registered for suite in the default registry.
func DialerFor(suite uint32) (Dialer, bool) { return defaultRegistry.DialerFor(suite) }

// ListenerFor returns the listener registered for suite in the default registry.
func ListenerFor(suite uint32) (Listener, bool) { return defaultRegistry.ListenerFor(suite) }
