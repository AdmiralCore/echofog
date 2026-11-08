package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// The VPN of the app (T11.4): a local SOCKS5 proxy whose tunnels go through exit/2 to one exit that a
// link names. The exits are free here (no market, no payment); the link is the exit's own card link.

// ExitLink is a connection to one exit (*exit.Client): a dialer of tunnels the local proxy hands out,
// closed when the VPN stops.
type ExitLink interface {
	DialExitStream(ctx context.Context, target string) (io.ReadWriteCloser, error)
	Close() error
}

// ExitDialer connects to the exit whose card it is given.
type ExitDialer interface {
	Dial(ctx context.Context, card *objects.Card) (ExitLink, error)
}

// VPN errors.
var (
	// ErrVPNRunning reports a start while the VPN runs.
	ErrVPNRunning = errors.New("app: the VPN is running")
	// ErrNotExit reports a link whose node offers no exit.
	ErrNotExit = errors.New("app: the node is no exit")
	// ErrVPNOff reports a VPN call on an app built without an exit dialer.
	ErrVPNOff = errors.New("app: the VPN is not available")
	// ErrVPNListen reports a proxy address that is not on the loopback: the proxy has no password.
	ErrVPNListen = errors.New("app: the proxy listens on the loopback only")
	// ErrNoExit reports an exit that is not in the list.
	ErrNoExit = errors.New("app: no such exit")
)

// Exit states of the VPN status: what the exit says about serving this node.
const (
	ExitChecking     = "checking"      // no tunnel has been tried yet
	ExitOK           = "ok"            // a tunnel went through
	ExitUnreachable  = "unreachable"   // no session to the exit could be set up
	ExitNoCamouflage = "no-camouflage" // the exit has no cover and could not be reached
)

const (
	exitPrefix     = "vpn/exit/"
	histPrefix     = "vpn/hist/"
	maxSavedExits  = 256
	maxVPNSessions = 100
	defaultListen  = "127.0.0.1:1080"
)

// VPNStatus is the state of the VPN.
type VPNStatus struct {
	Running bool
	Listen  string
	Exit    string // hex node ID of the exit
	Active  int64
	Rx, Tx  uint64
	// ExitState is ExitChecking, ExitOK, ExitUnreachable or ExitNoCamouflage.
	ExitState string
	Refused   uint64
	// RefusedBy counts refusals by the stable code of exit.Reason; LastReason is the latest one, for
	// LastHost ("host:port" the user's own program asked for) at LastAt (unix seconds).
	RefusedBy  map[string]uint64
	LastReason string
	LastHost   string
	LastAt     int64
}

// SavedExit is an exit the user has used or marked.
type SavedExit struct {
	Node             string
	Name             string
	Favorite, Active bool
	Sessions         int
	LastUsed         int64
	Rx, Tx, Refused  uint64
	RefusedBy        map[string]uint64
}

// VPNSession is one run of the VPN.
type VPNSession struct {
	Node               string
	StartedAt, EndedAt int64 // EndedAt 0 while it runs
	Rx, Tx, Refused    uint64
	RefusedBy          map[string]uint64
	EndReason          string // "stopped", "switched" or "app_closed"
}

type exitRec struct {
	Link      string            `json:"l"`
	Name      string            `json:"n,omitempty"`
	Favorite  bool              `json:"f,omitempty"`
	Sessions  int               `json:"s"`
	LastUsed  int64             `json:"u"`
	Rx        uint64            `json:"rx"`
	Tx        uint64            `json:"tx"`
	Refused   uint64            `json:"r"`
	RefusedBy map[string]uint64 `json:"rb,omitempty"`
}

type sessionRec struct {
	Node      string            `json:"n"`
	Started   int64             `json:"a"`
	Ended     int64             `json:"e,omitempty"`
	Rx        uint64            `json:"rx"`
	Tx        uint64            `json:"tx"`
	Refused   uint64            `json:"r"`
	RefusedBy map[string]uint64 `json:"rb,omitempty"`
	Reason    string            `json:"w,omitempty"`
}

// vpnState is the running VPN; every field is guarded by mu.
type vpnState struct {
	mu       sync.Mutex
	starting bool
	running  bool
	link     ExitLink
	srv      *core.SOCKS5Server
	exit     string
	listen   string
	state    string
	refused  uint64
	by       map[string]uint64
	last     struct {
		reason, host string
		at           int64
	}
	histKey string
	started int64
	seq     int
}

func (a *App) vpnOn() error {
	if a.cfg.Exit == nil {
		return ErrVPNOff
	}
	return nil
}

// VPNStart starts the local proxy through the exit a link names. The link is redeemed (the session to
// the exit is opened), and the node behind it must offer an exit. A running VPN is stopped first by
// the caller (ErrVPNRunning).
func (a *App) VPNStart(ctx context.Context, link string) (VPNStatus, error) {
	if err := a.vpnOn(); err != nil {
		return VPNStatus{}, err
	}
	v := &a.vpn
	v.mu.Lock()
	if v.running || v.starting {
		v.mu.Unlock()
		return VPNStatus{}, ErrVPNRunning
	}
	v.starting = true
	v.mu.Unlock()
	st, err := a.vpnStart(ctx, link)
	v.mu.Lock()
	v.starting = false
	v.mu.Unlock()
	return st, err
}

func (a *App) vpnStart(ctx context.Context, text string) (VPNStatus, error) {
	_, link, node, device, err := a.open(text)
	if err != nil {
		return VPNStatus{}, err
	}
	if node == a.cfg.Self {
		return VPNStatus{}, ErrSelf
	}
	peer, err := a.cfg.Links.Redeem(ctx, link)
	if err != nil {
		return VPNStatus{}, err
	}
	if [32]byte(peer.Node) != node || [32]byte(peer.Device) != device {
		return VPNStatus{}, errors.New("app: the session is not with the node of the link")
	}
	// The card in the link is the newest the exit gave out; the one of an older session may predate
	// the moment the owner turned the exit on.
	card := peer.Card
	if pv, perr := a.cfg.Links.Open(link); perr == nil && pv.Card != nil && pv.Card.ExitOffer() != nil {
		card = pv.Card
	}
	if card == nil || card.ExitOffer() == nil {
		return VPNStatus{}, ErrNotExit
	}
	el, err := a.cfg.Exit.Dial(ctx, card)
	if err != nil {
		return VPNStatus{}, err
	}
	listen := a.cfg.VPNListen
	if listen == "" {
		listen = defaultListen
	}
	v := &a.vpn
	srv := core.NewSOCKS5Server(listen, watch{a: a, link: el})
	if err := srv.Start(); err != nil {
		_ = el.Close()
		return VPNStatus{}, err
	}
	nodeHex := hex.EncodeToString(node[:])
	now := a.cfg.Clock.Now().Unix()
	v.mu.Lock()
	v.running, v.link, v.srv, v.exit, v.listen = true, el, srv, nodeHex, srv.Addr().String()
	v.state, v.refused, v.by = ExitChecking, 0, map[string]uint64{}
	v.last.reason, v.last.host, v.last.at = "", "", 0
	v.started = now
	v.seq++
	v.histKey = fmt.Sprintf("%s%020d-%06d", histPrefix, now, v.seq)
	key := v.histKey
	v.mu.Unlock()
	a.vpnRemember(nodeHex, link, now)
	a.vpnSession(key, sessionRec{Node: nodeHex, Started: now}) // the run in use is in the history at once
	return a.VPNStatus(), nil
}

// watch is the dialer the proxy uses: it passes the tunnels to the exit and watches what comes of them.
type watch struct {
	a    *App
	link ExitLink
}

func (w watch) DialExitStream(ctx context.Context, target string) (io.ReadWriteCloser, error) {
	conn, err := w.link.DialExitStream(ctx, target)
	if err != nil {
		w.a.vpnRefused(err, target)
		return nil, err
	}
	if vn, ok := conn.(core.VerdictNotifier); ok {
		return &verdictConn{ReadWriteCloser: conn, vn: vn, a: w.a, target: target}, nil
	}
	w.a.vpnOK()
	return conn, nil
}

// verdictConn is a tunnel whose exit answers after it was handed out: the answer is what counts.
type verdictConn struct {
	io.ReadWriteCloser
	vn     core.VerdictNotifier
	a      *App
	target string
}

func (c *verdictConn) OnVerdict(f func(error)) {
	c.vn.OnVerdict(func(err error) {
		if err != nil {
			c.a.vpnRefused(err, c.target)
		} else {
			c.a.vpnOK()
		}
		f(err)
	})
}

func (c *verdictConn) CloseWrite() error {
	if cw, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// vpnRefused counts a tunnel the exit did not carry. An error that is no refusal of the exit (the
// caller went away, the proxy is stopping) is not counted.
func (a *App) vpnRefused(err error, target string) {
	r := exit.ReasonOf(err)
	if r == exit.ReasonUnknown {
		return
	}
	code := r.String()
	v := &a.vpn
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.running {
		return
	}
	v.refused++
	v.by[code]++
	v.last.reason, v.last.host, v.last.at = code, target, a.cfg.Clock.Now().Unix()
	switch r {
	case exit.ReasonNoPath:
		v.state = ExitUnreachable
	case exit.ReasonNoCamouflage:
		v.state = ExitNoCamouflage
	}
}

func (a *App) vpnOK() {
	v := &a.vpn
	v.mu.Lock()
	if v.running {
		v.state = ExitOK
	}
	v.mu.Unlock()
}

// VPNStop stops the VPN; stopping a stopped VPN is no error. The run goes to the history.
func (a *App) VPNStop() error { return a.vpnStop("stopped") }

func (a *App) vpnStop(reason string) error {
	v := &a.vpn
	v.mu.Lock()
	if !v.running {
		v.mu.Unlock()
		return nil
	}
	srv, link, nodeHex, key, started := v.srv, v.link, v.exit, v.histKey, v.started
	_, rx, tx := srv.Stats()
	refused, by := v.refused, copyCounts(v.by)
	v.running, v.srv, v.link = false, nil, nil
	v.mu.Unlock()
	_ = srv.Stop()
	_ = link.Close()
	now := a.cfg.Clock.Now().Unix()
	a.vpnSession(key, sessionRec{Node: nodeHex, Started: started, Ended: now, Rx: rx, Tx: tx, Refused: refused, RefusedBy: by, Reason: reason})
	a.vpnTotals(nodeHex, rx, tx, refused, by)
	return nil
}

func copyCounts(in map[string]uint64) map[string]uint64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(in))
	for k, n := range in {
		out[k] = n
	}
	return out
}

// VPNStatus reports the VPN.
func (a *App) VPNStatus() VPNStatus {
	v := &a.vpn
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.running {
		return VPNStatus{}
	}
	active, rx, tx := v.srv.Stats()
	return VPNStatus{Running: true, Listen: v.listen, Exit: v.exit, Active: active, Rx: rx, Tx: tx, ExitState: v.state,
		Refused: v.refused, RefusedBy: copyCounts(v.by), LastReason: v.last.reason, LastHost: v.last.host, LastAt: v.last.at}
}

// ---- the exits and the history -------------------------------------------------------------------------------

func (a *App) loadExit(node string) (exitRec, error) {
	raw, err := a.cfg.Store.Get([]byte(exitPrefix + node))
	if err != nil {
		return exitRec{}, ErrNoExit
	}
	var r exitRec
	if json.Unmarshal(raw, &r) != nil {
		return exitRec{}, ErrNoExit
	}
	return r, nil
}

func (a *App) storeExit(node string, r exitRec) {
	if raw, err := json.Marshal(r); err == nil {
		_ = a.cfg.Store.Put([]byte(exitPrefix+node), raw)
	}
}

// vpnRemember records that an exit was started: its link (to connect again) and a run more.
func (a *App) vpnRemember(node, link string, now int64) {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	r, err := a.loadExit(node)
	if err != nil {
		if es, serr := a.cfg.Store.Scan([]byte(exitPrefix), maxSavedExits); serr != nil || len(es) >= maxSavedExits {
			return
		}
	}
	r.Link, r.Sessions, r.LastUsed = link, r.Sessions+1, now
	a.storeExit(node, r)
}

// vpnTotals adds the traffic and the refusals of an ended run to its exit.
func (a *App) vpnTotals(node string, rx, tx, refused uint64, by map[string]uint64) {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	r, err := a.loadExit(node)
	if err != nil {
		return
	}
	r.Rx, r.Tx, r.Refused = r.Rx+rx, r.Tx+tx, r.Refused+refused
	if len(by) > 0 {
		if r.RefusedBy == nil {
			r.RefusedBy = map[string]uint64{}
		}
		for k, n := range by {
			r.RefusedBy[k] += n
		}
	}
	a.storeExit(node, r)
}

func (a *App) vpnSession(key string, s sessionRec) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	a.vmu.Lock()
	defer a.vmu.Unlock()
	_ = a.cfg.Store.Put([]byte(key), raw)
	es, err := a.cfg.Store.Scan([]byte(histPrefix), maxVPNSessions+64)
	if err != nil || len(es) <= maxVPNSessions {
		return
	}
	for _, e := range es[:len(es)-maxVPNSessions] { // the oldest go: the keys run by start time
		_ = a.cfg.Store.Delete(e.Key)
	}
}

// VPNExits lists the exits the user has used, the latest first. The one in use is marked Active.
func (a *App) VPNExits() []SavedExit {
	es, err := a.cfg.Store.Scan([]byte(exitPrefix), maxSavedExits)
	if err != nil {
		return nil
	}
	active := a.VPNStatus().Exit
	out := make([]SavedExit, 0, len(es))
	for _, e := range es {
		var r exitRec
		if json.Unmarshal(e.Value, &r) != nil {
			continue
		}
		node := string(e.Key[len(exitPrefix):])
		out = append(out, SavedExit{Node: node, Name: r.Name, Favorite: r.Favorite, Active: node == active, Sessions: r.Sessions,
			LastUsed: r.LastUsed, Rx: r.Rx, Tx: r.Tx, Refused: r.Refused, RefusedBy: r.RefusedBy})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastUsed != out[j].LastUsed {
			return out[i].LastUsed > out[j].LastUsed
		}
		return out[i].Node < out[j].Node
	})
	return out
}

func (a *App) updateExit(node string, fn func(*exitRec)) error {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	r, err := a.loadExit(node)
	if err != nil {
		return err
	}
	fn(&r)
	a.storeExit(node, r)
	return nil
}

// VPNName gives an exit a name of the user's own.
func (a *App) VPNName(node, name string) error {
	if name != "" {
		var err error
		if name, err = cleanAlias(name); err != nil {
			return err
		}
	}
	return a.updateExit(node, func(r *exitRec) { r.Name = name })
}

// VPNFavorite marks an exit or unmarks it.
func (a *App) VPNFavorite(node string, on bool) error {
	return a.updateExit(node, func(r *exitRec) { r.Favorite = on })
}

// VPNForget drops an exit from the list; the exit in use stays until the VPN stops.
func (a *App) VPNForget(node string) error {
	if a.VPNStatus().Exit == node {
		return ErrVPNRunning
	}
	if _, err := a.loadExit(node); err != nil {
		return err
	}
	return a.cfg.Store.Delete([]byte(exitPrefix + node))
}

// VPNStartSaved connects to an exit of the list: its link is redeemed again.
func (a *App) VPNStartSaved(ctx context.Context, node string) (VPNStatus, error) {
	r, err := a.loadExit(node)
	if err != nil {
		return VPNStatus{}, err
	}
	return a.VPNStart(ctx, r.Link)
}

// VPNHistory lists the runs of the VPN, the latest first.
func (a *App) VPNHistory() []VPNSession {
	es, err := a.cfg.Store.Scan([]byte(histPrefix), maxVPNSessions+64)
	if err != nil {
		return nil
	}
	out := make([]VPNSession, 0, len(es))
	for i := len(es) - 1; i >= 0; i-- {
		var s sessionRec
		if json.Unmarshal(es[i].Value, &s) != nil {
			continue
		}
		out = append(out, VPNSession{Node: s.Node, StartedAt: s.Started, EndedAt: s.Ended, Rx: s.Rx, Tx: s.Tx, Refused: s.Refused,
			RefusedBy: s.RefusedBy, EndReason: s.Reason})
	}
	return out
}

// VPNClearHistory forgets the runs; the exits and their totals stay.
func (a *App) VPNClearHistory() error {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	es, err := a.cfg.Store.Scan([]byte(histPrefix), maxVPNSessions+64)
	if err != nil {
		return err
	}
	for _, e := range es {
		if err := a.cfg.Store.Delete(e.Key); err != nil {
			return err
		}
	}
	return nil
}

// checkLoopback refuses a proxy address that is not on the loopback: the SOCKS5 entrance has no password,
// so "0.0.0.0" would open it to the network. Empty means the default.
func checkLoopback(addr string) error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return ErrVPNListen
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return ErrVPNListen
	}
	return nil
}
