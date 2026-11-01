package network

import "net"

// ListenHider is an authenticator that can stop announcing the listen port to some peers.
type ListenHider interface {
	SetHideListenWhen(fn func(remote net.Addr) bool)
}

// SetHideListenWhen makes the node announce no listen port to the peer at remote while fn reports
// true, so that peer keeps the node's address to itself (nil: never). A node in guard mode uses
// it: its address, spread through the mesh, would bring it direct noise links from across the
// border that it has to refuse.
func (a *DefaultPeerAuthenticator) SetHideListenWhen(fn func(remote net.Addr) bool) {
	if fn == nil {
		a.hideWhen.Store(nil)
		return
	}
	a.hideWhen.Store(&fn)
}

// ListenHidden reports whether the node announces no port to the peer at remote: a hidden node, or
// the condition of SetHideListenWhen holds for it.
func (a *DefaultPeerAuthenticator) ListenHidden(remote net.Addr) bool {
	if a.hideListen.Load() {
		return true
	}
	fn := a.hideWhen.Load()
	return fn != nil && remote != nil && (*fn)(remote)
}

// announcedPort is the port a TCP dial to remote announces once authenticated: none while the
// authenticator hides the node's port from it (the handshake leaves it out then too).
func (t *TCPTransport) announcedPort(remote net.Addr) uint16 {
	if h, ok := t.auth.(interface{ ListenHidden(net.Addr) bool }); ok && h.ListenHidden(remote) {
		return 0
	}
	return uint16(t.advertisedPort.Load())
}

// SetHideListenWhen: see DefaultPeerAuthenticator.SetHideListenWhen.
func (q *QuicTransport) SetHideListenWhen(fn func(remote net.Addr) bool) {
	if h, ok := q.authenticator.(ListenHider); ok {
		h.SetHideListenWhen(fn)
	}
}

// SetHideListenWhen: see DefaultPeerAuthenticator.SetHideListenWhen.
func (t *TCPTransport) SetHideListenWhen(fn func(remote net.Addr) bool) {
	if h, ok := t.auth.(ListenHider); ok {
		h.SetHideListenWhen(fn)
	}
}

// SetHideListenWhen: see DefaultPeerAuthenticator.SetHideListenWhen; on both transports.
func (f *FallbackTransport) SetHideListenWhen(fn func(remote net.Addr) bool) {
	f.QUIC.SetHideListenWhen(fn)
	if f.TCP != nil {
		f.TCP.SetHideListenWhen(fn)
	}
}

// SetHideListenWhen: see DefaultPeerAuthenticator.SetHideListenWhen.
func (t *WSTransport) SetHideListenWhen(fn func(remote net.Addr) bool) {
	if h, ok := t.auth.(ListenHider); ok {
		h.SetHideListenWhen(fn)
	}
}
