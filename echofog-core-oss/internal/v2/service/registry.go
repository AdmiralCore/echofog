package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Handler serves one stream of a service. It owns the stream and must end it.
type Handler func(ctx context.Context, st *mux.Stream)

// Offer is one entry of HsInit.services: ServiceOffer { uint32 id = 1; repeated uint32 versions = 2; }.
type Offer struct {
	ID       uint64
	Versions []uint64 // ascending
}

// Choice is one entry of HsResp.services: ServiceChoice { uint32 id = 1; uint32 version = 2; }.
type Choice struct {
	ID      uint64
	Version uint64
}

type registered struct {
	spec     Spec
	versions map[uint64]Handler // nil handler: usable as a client, not served
	served   map[uint64]bool
}

// Registry is the set of services and versions this node can use and serve.
type Registry struct {
	mu       sync.RWMutex
	services map[uint64]*registered
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{services: map[uint64]*registered{}} }

func (r *Registry) add(sp Spec, version uint64, h Handler, serve bool) {
	if _, ok := sp.Versions[version]; !ok {
		panic(fmt.Sprintf("service: %s has no version %d", sp.Name, version))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.services[sp.ID]
	if e == nil {
		e = &registered{spec: Spec{ID: sp.ID, Name: sp.Name, Versions: map[uint64]Version{}},
			versions: map[uint64]Handler{}, served: map[uint64]bool{}}
		r.services[sp.ID] = e
	}
	e.spec.Versions[version] = sp.Versions[version]
	if serve {
		e.versions[version] = h
		e.served[version] = true
	} else if _, ok := e.versions[version]; !ok {
		e.versions[version] = nil
	}
}

// Serve registers a built-in service version with its handler: this node offers and serves it.
func (r *Registry) Serve(id, version uint64, h Handler) {
	sp, ok := Builtin(id)
	if !ok {
		panic(fmt.Sprintf("service: %d is not a built-in service", id))
	}
	r.add(sp, version, h, true)
}

// ServeSpec registers a service version that is not built in (a later or experimental service).
func (r *Registry) ServeSpec(sp Spec, version uint64, h Handler) { r.add(sp, version, h, true) }

// Remove stops serving and offering a service: its handlers go and it leaves the offer of new
// sessions. Sessions already open keep what they negotiated.
func (r *Registry) Remove(id uint64) {
	r.mu.Lock()
	delete(r.services, id)
	r.mu.Unlock()
}

// Use registers a built-in service version this node opens as a client but does not serve.
func (r *Registry) Use(id, version uint64) {
	sp, ok := Builtin(id)
	if !ok {
		panic(fmt.Sprintf("service: %d is not a built-in service", id))
	}
	r.add(sp, version, nil, false)
}

// UseSpec is Use for a service that is not built in.
func (r *Registry) UseSpec(sp Spec, version uint64) { r.add(sp, version, nil, false) }

// Offer returns HsInit.services: every service this node can use or serve, ascending by ID,
// versions ascending; with grease, one GREASE service ID with version 1 in its numeric place
// (D18).
func (r *Registry) Offer(grease bool) []Offer {
	r.mu.RLock()
	out := make([]Offer, 0, len(r.services)+1)
	for id, e := range r.services {
		o := Offer{ID: id}
		for v := range e.versions {
			o.Versions = append(o.Versions, v)
		}
		slices.Sort(o.Versions)
		out = append(out, o)
	}
	r.mu.RUnlock()
	if grease {
		out = append(out, Offer{ID: uint64(RandomGrease()), Versions: []uint64{1}})
	}
	slices.SortFunc(out, func(a, b Offer) int { return cmpU64(a.ID, b.ID) })
	return out
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Select is the responder's choice for HsResp.services: for each offered service it serves,
// the highest common version. GREASE IDs, unknown services and services it does not serve are
// ignored (an empty selection means the service is not offered to this peer).
func (r *Registry) Select(offers []Offer) []Choice {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Choice
	seen := map[uint64]bool{}
	for _, o := range offers {
		if IsGreaseService(o.ID) || seen[o.ID] {
			continue
		}
		seen[o.ID] = true
		e := r.services[o.ID]
		if e == nil {
			continue
		}
		var best uint64
		found := false
		for _, v := range o.Versions {
			if e.served[v] && (!found || v > best) {
				best, found = v, true
			}
		}
		if found {
			out = append(out, Choice{ID: o.ID, Version: best})
		}
	}
	slices.SortFunc(out, func(a, b Choice) int { return cmpU64(a.ID, b.ID) })
	return out
}

// VerifyChoices is the initiator's check of HsResp.services: every choice must be a version it
// offered for that service, one choice per service (handshake.md §7).
func VerifyChoices(offered []Offer, choices []Choice) error {
	seen := map[uint64]bool{}
	for _, c := range choices {
		if seen[c.ID] {
			return fmt.Errorf("%w: service %d chosen twice", ErrDowngrade, c.ID)
		}
		seen[c.ID] = true
		i := slices.IndexFunc(offered, func(o Offer) bool { return o.ID == c.ID })
		if i < 0 || IsGreaseService(c.ID) || !slices.Contains(offered[i].Versions, c.Version) {
			return fmt.Errorf("%w: service %d version %d", ErrDowngrade, c.ID, c.Version)
		}
	}
	return nil
}

// Policy is the service view of one session: the selected (service, version) pairs. It
// implements mux.StreamPolicy.
//
// A selected pair is usable in both directions; a side without a handler answers
// UNSUPPORTED{service, 0, 0} (wire.md §6.3, services.md §1).
type Policy struct {
	reg    *Registry
	chosen map[uint64]uint64
}

// Policy returns the policy of a session with these choices.
func (r *Registry) Policy(choices []Choice) *Policy {
	p := &Policy{reg: r, chosen: map[uint64]uint64{}}
	for _, c := range choices {
		p.chosen[c.ID] = c.Version
	}
	return p
}

// Limiter implements mux.StreamPolicy.
func (p *Policy) Limiter(service, version uint64) (frame.Limiter, bool) {
	v, ok := p.chosen[service]
	if !ok || v != version {
		return nil, false
	}
	p.reg.mu.RLock()
	defer p.reg.mu.RUnlock()
	e := p.reg.services[service]
	if e == nil {
		return nil, false
	}
	ver, ok := e.spec.Versions[version]
	if !ok {
		return nil, false
	}
	return ver.Limiter(), true
}

// Selected returns the selected version of a service.
func (p *Policy) Selected(service uint64) (uint64, bool) {
	v, ok := p.chosen[service]
	return v, ok
}

// handler returns the local handler of a selected pair, if this side serves it.
func (p *Policy) handler(service, version uint64) Handler {
	if p.chosen[service] != version {
		return nil
	}
	p.reg.mu.RLock()
	defer p.reg.mu.RUnlock()
	if e := p.reg.services[service]; e != nil && e.served[version] {
		return e.versions[version]
	}
	return nil
}

// served returns the versions this side serves to this peer for UNSUPPORTED: the selected
// version if it serves it, else 0,0 (none).
func (p *Policy) served(service uint64) (uint64, uint64) {
	v, ok := p.chosen[service]
	if !ok {
		return 0, 0
	}
	p.reg.mu.RLock()
	defer p.reg.mu.RUnlock()
	if e := p.reg.services[service]; e != nil && e.served[v] && e.versions[v] != nil {
		return v, v
	}
	return 0, 0
}

// ErrNotSelected: the service was not selected for this session.
var ErrNotSelected = errors.New("service: not selected for this session")

// Open opens a stream of the selected version of a service.
func Open(ctx context.Context, s *mux.Session, p *Policy, service uint64) (*mux.Stream, error) {
	v, ok := p.chosen[service]
	if !ok {
		return nil, ErrNotSelected
	}
	return s.OpenStream(ctx, service, v)
}

// Serve accepts the session's incoming streams and dispatches each to its handler. A stream
// whose (service, version) was not selected, or that this side does not serve, is answered with
// UNSUPPORTED{service, min, max} and ends (wire.md §6.3); this is never abuse. Serve returns when
// the session or ctx ends.
func Serve(ctx context.Context, s *mux.Session, p *Policy) error {
	for {
		st, err := s.AcceptStream(ctx)
		if err != nil {
			return err
		}
		o := st.Open()
		var h Handler
		if st.Selected() {
			h = p.handler(o.Service, o.Version)
		}
		if h == nil {
			go unsupported(st, p)
			continue
		}
		go h(ctx, st)
	}
}

// unsupported answers UNSUPPORTED and ends the stream with a normal FIN, never a reset, which
// could discard the UNSUPPORTED frame (wire.md §6.3).
func unsupported(st *mux.Stream, p *Policy) {
	o := st.Open()
	min, max := p.served(o.Service)
	st.WriteFrame(frame.TypeUnsupported, 0, frame.Unsupported{Service: o.Service, Min: min, Max: max}.Append(nil))
	st.Close()
}
