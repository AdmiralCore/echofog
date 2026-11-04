package core

import "time"

// Test-only views of a session pool and the cached pools.

func (p *PeerSessionPool) setDrainTimeout(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drainTimeout = d
}

func (p *PeerSessionPool) activeSessions() []*ManagedSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*ManagedSession(nil), p.active...)
}

func (p *PeerSessionPool) drainingSessions() []*ManagedSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*ManagedSession(nil), p.draining...)
}
