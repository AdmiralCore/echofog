package app

// PeerStatus is one live session as the network screen shows it. It has no address.
type PeerStatus struct {
	Device string // short form
	Path   string // "direct" | "relay"
	Paths  int
}

// NetStatus is the network screen.
type NetStatus struct {
	Running  bool
	Mode     Mode
	NetClass string
	Sessions int
	Direct   int
	Relay    int
	Peers    []PeerStatus
}

// NetStatus reports sessions, paths and the route policy.
func (a *App) NetStatus() NetStatus { return NetStatus{} }

// Report is the connection report of the "Отчёт" button: no addresses, ids, links or keys.
func (a *App) Report() string { return "" }
