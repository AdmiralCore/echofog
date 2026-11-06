package app

import (
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
)

// PeerStatus is one live session as the network screen shows it. It has no address and only a
// short form of the device ID.
type PeerStatus struct {
	Device string
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

func pathName(k mux.PathKind) string {
	if k == mux.PathRelay {
		return "relay"
	}
	return "direct"
}

// NetStatus reports sessions, paths and the route policy class.
func (a *App) NetStatus() NetStatus {
	st := a.cfg.Runtime.Status()
	out := NetStatus{Running: a.running.Load(), Mode: a.cfg.Mode, NetClass: a.cfg.Runtime.NetClass().String(), Sessions: len(st.Sessions)}
	for _, s := range st.Sessions {
		p := PeerStatus{Device: fmt.Sprintf("%x", s.Device[:4]), Path: pathName(s.Path), Paths: len(s.Paths)}
		if s.Path == mux.PathRelay {
			out.Relay++
		} else {
			out.Direct++
		}
		out.Peers = append(out.Peers, p)
	}
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].Device < out.Peers[j].Device })
	return out
}

// Report is the connection report of the "Отчёт" button: what a tester posts when EchoFog does not
// connect, with no address, no ID, no key, no link and no contact. Nothing is sent anywhere.
func (a *App) Report() string {
	st := a.cfg.Runtime.Status()
	ns := a.NetStatus()
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	version := a.cfg.Version
	if version == "" {
		version = "unknown"
	}
	line("EchoFog connection report — no addresses, IDs, links or contacts")
	line("app: %s  protocol: 2  os: %s/%s", version, runtime.GOOS, runtime.GOARCH)
	line("mode: %s  platform: %s  network: %s", ns.Mode, st.Profile.Platform, ns.NetClass)
	if next := a.NextMode(); next != "" {
		line("mode after restart: %s", next)
	}
	paths, standby := 0, 0
	suites := map[uint16]int{}
	for _, s := range st.Sessions {
		paths += len(s.Paths)
		standby += s.Standby
		suites[s.Suite]++
	}
	line("running: %s", yesNo(ns.Running))
	line("sessions: %d  direct %d  relay %d  paths: %d  standby: %d", ns.Sessions, ns.Direct, ns.Relay, paths, standby)
	a.routeLines(st, line)
	line("birthday punch: tried %d, direct %d", st.Birthday.Tried, st.Birthday.OK)
	ids := make([]int, 0, len(suites))
	for s := range suites {
		ids = append(ids, int(s))
	}
	sort.Ints(ids)
	for _, s := range ids {
		line("suite 0x%04x: %d", s, suites[uint16(s)])
	}
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// routeLines tells which route-policy rule applied to the sessions and which table it came from
// (T6.4): rule IDs and the table version only, no address.
func (a *App) routeLines(st node.Status, line func(string, ...any)) {
	rules := map[string]int{}
	tables := map[string]bool{}
	for _, s := range st.Sessions {
		if r, ok := a.cfg.Runtime.RouteReport(s.Device); ok {
			rules[r.RuleID]++
			tables[r.TableVersion] = true
		}
	}
	if len(rules) == 0 {
		line("route: no decision yet")
		return
	}
	ids := make([]string, 0, len(rules))
	for id := range rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := rules[id]
		word := "sessions"
		if n == 1 {
			word = "session"
		}
		line("route rule %s: %d %s", id, n, word)
	}
	vs := make([]string, 0, len(tables))
	for v := range tables {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	line("route table: %s", strings.Join(vs, ", "))
}
