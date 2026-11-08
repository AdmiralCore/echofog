//go:build windows

package winwfp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/tailscale/wf"
	"golang.org/x/sys/windows"
)

// engine is the part of *wf.Session the driver uses; tests replace it.
type engine interface {
	Providers() ([]*wf.Provider, error)
	AddProvider(*wf.Provider) error
	DeleteProvider(wf.ProviderID) error
	Sublayers(...wf.ProviderID) ([]*wf.Sublayer, error)
	AddSublayer(*wf.Sublayer) error
	DeleteSublayer(wf.SublayerID) error
	Rules() ([]*wf.Rule, error)
	AddRule(*wf.Rule) error
	DeleteRule(wf.RuleID) error
	Close() error
}

// fwpEAccessDenied is FWP_E_ACCESS_DENIED, the answer of the BFE to a caller without the right.
const fwpEAccessDenied = syscall.Errno(0x80320003)

// openEngine opens a non-dynamic session: what it adds outlives the process.
func openEngine() (engine, error) {
	s, err := wf.New(&wf.Options{Name: "EchoFog kill switch", Description: providerDesc})
	if err != nil {
		return nil, mapErr(err)
	}
	return s, nil
}

// mapErr turns the BFE's refusal into tun.ErrNoPrivilege.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, fwpEAccessDenied) {
		return fmt.Errorf("%w: %v", tun.ErrNoPrivilege, err)
	}
	return err
}

// Weights of the filters: the rule weight of package tun, as it is (1 block-all, 10 permits, 20
// endpoints, 30 DNS blocks). Within the sublayer the filter of the highest weight that matches
// decides, so the order of package tun is the order in the kernel.
func weightOf(r tun.Rule) uint64 { return uint64(r.Weight) }

func layerOf(dir tun.Dir, v6 bool) wf.LayerID {
	switch {
	case dir == tun.Out && !v6:
		return wf.LayerALEAuthConnectV4
	case dir == tun.Out:
		return wf.LayerALEAuthConnectV6
	case !v6:
		return wf.LayerALEAuthRecvAcceptV4
	}
	return wf.LayerALEAuthRecvAcceptV6
}

// Compile turns rules into WFP filters (not yet added). luid is the interface LUID of the tunnel
// adapter, 0 while there is none: rules for the tunnel are then left out, which keeps the
// computer closed. newID makes the key of each filter.
//
// The filters live in the ALE layers: AUTH_CONNECT for what this computer starts and
// AUTH_RECV_ACCEPT for what arrives. "Physical" is every interface that is not the tunnel and not
// loopback. For ICMP the type is carried by the local-port field, which is how WFP names it.
func Compile(rules []tun.Rule, luid uint64, newID func() windows.GUID) ([]*wf.Rule, error) {
	var out []*wf.Rule
	for _, r := range rules {
		if r.Iface == tun.IfaceTunnel && luid == 0 {
			continue
		}
		var cs []*wf.Match
		add := func(f wf.FieldID, op wf.MatchType, v interface{}) {
			cs = append(cs, &wf.Match{Field: f, Op: op, Value: v})
		}
		switch r.Iface {
		case tun.IfaceLoopback:
			add(wf.FieldFlags, wf.MatchTypeFlagsAllSet, wf.ConditionFlagIsLoopback)
		case tun.IfaceTunnel:
			add(wf.FieldIPLocalInterface, wf.MatchTypeEqual, luid)
		case tun.IfacePhysical:
			if luid != 0 {
				add(wf.FieldIPLocalInterface, wf.MatchTypeNotEqual, luid)
			}
			add(wf.FieldFlags, wf.MatchTypeFlagsNoneSet, wf.ConditionFlagIsLoopback)
		}
		switch r.Proto {
		case tun.ProtoTCP:
			add(wf.FieldIPProtocol, wf.MatchTypeEqual, wf.IPProtoTCP)
		case tun.ProtoUDP:
			add(wf.FieldIPProtocol, wf.MatchTypeEqual, wf.IPProtoUDP)
		case tun.ProtoICMP:
			p := wf.IPProtoICMP
			if r.V6 {
				p = wf.IPProtoICMPV6
			}
			add(wf.FieldIPProtocol, wf.MatchTypeEqual, p)
		}
		if r.Remote.IsValid() {
			add(wf.FieldIPRemoteAddress, wf.MatchTypeEqual, r.Remote)
		}
		if r.RemotePort != 0 {
			add(wf.FieldIPRemotePort, wf.MatchTypeEqual, r.RemotePort)
		}
		switch {
		case r.ICMPHi != 0:
			add(wf.FieldIPLocalPort, wf.MatchTypeRange, wf.Range{From: uint16(r.ICMPLo), To: uint16(r.ICMPHi)})
		case r.LocalPort != 0:
			add(wf.FieldIPLocalPort, wf.MatchTypeEqual, r.LocalPort)
		}
		act := wf.ActionBlock
		if r.Action == tun.Permit {
			act = wf.ActionPermit
		}
		out = append(out, &wf.Rule{
			ID:          wf.RuleID(newID()),
			Name:        "EchoFog: " + r.Name,
			Description: "EchoFog kill switch",
			Layer:       layerOf(r.Dir, r.V6),
			Sublayer:    SublayerKey,
			Weight:      weightOf(r),
			Conditions:  cs,
			Action:      act,
			Persistent:  true,
			Provider:    ProviderKey,
		})
	}
	return out, nil
}

func randomGUID() windows.GUID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	// version 4, variant 10
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	g := windows.GUID{Data1: uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), Data2: uint16(b[4])<<8 | uint16(b[5]), Data3: uint16(b[6])<<8 | uint16(b[7])}
	copy(g.Data4[:], b[8:])
	return g
}

// Firewall is the tun.Firewall of Windows.
type Firewall struct {
	open  func() (engine, error)
	newID func() windows.GUID
	// marker is the file whose presence says "filters are installed" to a process that may not read
	// the BFE (the unelevated app at start-up).
	marker string

	mu   sync.Mutex
	luid uint64
	last []tun.Rule
}

// NewFirewall returns the driver over the real Base Filtering Engine. It needs administrator
// rights for Install and Remove.
func NewFirewall() *Firewall {
	return &Firewall{open: openEngine, newID: randomGUID, marker: MarkerPath()}
}

// MarkerPath is where the elevated helper records that the filters stand:
// %ProgramData%\EchoFog\killswitch.on. A user may read it; only administrators may write there.
func MarkerPath() string {
	dir := os.Getenv("ProgramData")
	if dir == "" {
		dir = `C:\ProgramData`
	}
	return filepath.Join(dir, "EchoFog", "killswitch.on")
}

func (f *Firewall) setMarker(on bool) {
	if f.marker == "" {
		return
	}
	if on {
		_ = os.MkdirAll(filepath.Dir(f.marker), 0o755)
		_ = os.WriteFile(f.marker, []byte("EchoFog kill switch filters are installed. Remove: efv2 killswitch remove\n"), 0o644)
		return
	}
	_ = os.Remove(f.marker)
}

// ensure creates the provider and the sublayer when they are not there yet.
func ensure(e engine) error {
	ps, err := e.Providers()
	if err != nil {
		return mapErr(err)
	}
	have := false
	for _, p := range ps {
		have = have || p.ID == ProviderKey
	}
	if !have {
		if err := e.AddProvider(&wf.Provider{ID: ProviderKey, Name: providerName, Description: providerDesc, Persistent: true}); err != nil {
			return mapErr(err)
		}
	}
	ss, err := e.Sublayers()
	if err != nil {
		return mapErr(err)
	}
	have = false
	for _, s := range ss {
		have = have || s.ID == SublayerKey
	}
	if !have {
		if err := e.AddSublayer(&wf.Sublayer{ID: SublayerKey, Name: sublayerName, Description: providerDesc, Persistent: true,
			Provider: ProviderKey, Weight: SublayerWeight}); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func ownRules(e engine) ([]*wf.Rule, error) {
	all, err := e.Rules()
	if err != nil {
		return nil, mapErr(err)
	}
	var mine []*wf.Rule
	for _, r := range all {
		if r.Provider == ProviderKey {
			mine = append(mine, r)
		}
	}
	return mine, nil
}

// install replaces the filters: the new ones are added first and the old ones deleted after, so
// there is no moment with fewer blocks. For a short time both sets stand, which can only mean the
// permits of the old plan are there next to the new ones. A failure takes the new filters back.
func (f *Firewall) install(e engine, rules []tun.Rule, luid uint64) error {
	if err := ensure(e); err != nil {
		return err
	}
	old, err := ownRules(e)
	if err != nil {
		return err
	}
	fresh, err := Compile(rules, luid, f.newID)
	if err != nil {
		return err
	}
	for i, r := range fresh {
		if err := e.AddRule(r); err != nil {
			for _, done := range fresh[:i] {
				_ = e.DeleteRule(done.ID)
			}
			return mapErr(fmt.Errorf("add filter %q: %w", r.Name, err))
		}
	}
	var failed error
	for _, r := range old {
		if err := e.DeleteRule(r.ID); err != nil && failed == nil {
			failed = err
		}
	}
	if failed != nil {
		// The new set stands and the old one is partly there: closed, though not tidy.
		return fmt.Errorf("winwfp: old filters stay: %w", failed)
	}
	return nil
}

// SetTunnelLUID tells the driver which adapter is the tunnel; the installed rules are replaced with
// ones that permit it. Call it after the adapter exists (and with 0 after it is gone).
func (f *Firewall) SetTunnelLUID(luid uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.luid = luid
	if len(f.last) == 0 {
		return nil
	}
	e, err := f.open()
	if err != nil {
		return err
	}
	defer e.Close()
	return f.install(e, f.last, luid)
}

func (f *Firewall) Install(ctx context.Context, rules []tun.Rule) error {
	// A second line of defence: this process also gets its rules from another one.
	if err := tun.CheckRules(rules); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.open()
	if err != nil {
		return err
	}
	defer e.Close()
	if err := f.install(e, rules, f.luid); err != nil {
		return err
	}
	f.last = append([]tun.Rule(nil), rules...)
	f.setMarker(true)
	return nil
}

func (f *Firewall) Remove(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, err := f.open()
	if err != nil {
		return err
	}
	defer e.Close()
	if _, err := removeAll(e); err != nil {
		return err
	}
	f.last = nil
	f.setMarker(false)
	return nil
}

// Installed answers from the system: the marker of an earlier elevated run, or the BFE itself when
// the caller may read it. It errs on the side of "yes".
func (f *Firewall) Installed(ctx context.Context) (bool, error) {
	if f.marker != "" {
		if _, err := os.Stat(f.marker); err == nil {
			return true, nil
		}
	}
	e, err := f.open()
	if err != nil {
		if errors.Is(err, tun.ErrNoPrivilege) {
			return false, nil // cannot look, and no marker: nothing is known to stand
		}
		return false, err
	}
	defer e.Close()
	mine, err := ownRules(e)
	if err != nil {
		if errors.Is(err, tun.ErrNoPrivilege) {
			return false, nil
		}
		return false, err
	}
	return len(mine) > 0, nil
}

// removeAll deletes every filter of the provider, then the sublayer and the provider. A step that
// fails does not stop the others; the first error is returned, and the filters count is what was
// removed.
func removeAll(e engine) (int, error) {
	mine, err := ownRules(e)
	if err != nil {
		return 0, err
	}
	var first error
	n := 0
	for _, r := range mine {
		if err := e.DeleteRule(r.ID); err != nil {
			if first == nil {
				first = fmt.Errorf("delete filter %q: %w", r.Name, err)
			}
			continue
		}
		n++
	}
	if first != nil {
		return n, first
	}
	ss, err := e.Sublayers()
	if err != nil {
		return n, mapErr(err)
	}
	for _, s := range ss {
		if s.ID == SublayerKey {
			if err := e.DeleteSublayer(SublayerKey); err != nil {
				return n, mapErr(err)
			}
		}
	}
	ps, err := e.Providers()
	if err != nil {
		return n, mapErr(err)
	}
	for _, p := range ps {
		if p.ID == ProviderKey {
			if err := e.DeleteProvider(ProviderKey); err != nil {
				return n, mapErr(err)
			}
		}
	}
	return n, nil
}

// RemoveAll deletes every object of the kill switch and returns how many filters it removed. It is
// the manual removal (efv2 killswitch remove) and needs administrator rights. On a computer that
// has none of the objects it removes nothing and succeeds.
func RemoveAll() (int, error) {
	e, err := openEngine()
	if err != nil {
		return 0, err
	}
	defer e.Close()
	n, err := removeAll(e)
	if err == nil {
		_ = os.Remove(MarkerPath())
	}
	return n, err
}

var _ tun.Firewall = (*Firewall)(nil)
