package node

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
)

// Automatic exit switching. While the proxy runs, a watcher looks at the exit every few seconds;
// when it is gone, refuses to serve us, or no site opens through it any more, the proxy moves to
// the next exit that answers and serves, without closing the local port: applications (and the
// TUN in whole-computer mode) keep working and only see a few failed connections.

const (
	failoverEvery    = 3 * time.Second
	failoverGrace    = 12 * time.Second // a freshly chosen exit is not judged before this
	failoverAvoidFor = 10 * time.Minute // a failed exit is not chosen again for this long
	failStreakLimit  = 4                // streams in a row failing because of the exit ...
	failHostsLimit   = 2                // ... over at least this many sites
	failNoSuccessFor = 20 * time.Second // ... with nothing working for this long
	lostChecks       = 2                // rounds without the peer link before it counts as gone
	pickTries        = 4                // exits asked per search
	refreshAtMostAt  = time.Minute      // the network is searched for exits at most this often
	reviveEvery      = 15 * time.Second // with no other exit, the one in use is redialed this often
	stuckLogEvery    = time.Minute      // "no other exit" is logged again at most this often
)

// Why the proxy left an exit.
const (
	SwitchLost     = "lost"        // the peer link to the exit is gone
	SwitchDisabled = "disabled"    // the exit stopped sharing its connection
	SwitchNoAnswer = "unreachable" // the exit does not answer exit requests
	SwitchFailing  = "failing"     // no site opens through it any more
)

// exitCheck is what the watcher knows about the exit in use.
type exitCheck struct {
	Lost   int    // rounds in a row without the peer link
	State  string // VPNStatus.ExitState
	Health exit_gateway.StreamHealth
	Using  time.Duration // since the proxy moved to this exit
}

// exitFailure says why the exit in use should be left, or "" while it works.
func exitFailure(c exitCheck, now time.Time) string {
	if c.State == ExitDisabled {
		return SwitchDisabled // said by the exit itself: no need to wait
	}
	if c.Using < failoverGrace {
		return ""
	}
	switch {
	case c.Lost >= lostChecks:
		return SwitchLost
	case c.State == ExitUnreachable:
		return SwitchNoAnswer
	case c.Health.FailStreak >= failStreakLimit && c.Health.FailHosts >= failHostsLimit &&
		(c.Health.LastOK.IsZero() || now.Sub(c.Health.LastOK) > failNoSuccessFor):
		return SwitchFailing
	}
	return ""
}

func (n *NodeApi) autoSwitchOn() bool {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return !n.noAutoSwitch
}

// GetVPNAutoSwitch tells whether the proxy moves to another exit by itself when one stops working.
func (n *NodeApi) GetVPNAutoSwitch() bool { return n.autoSwitchOn() }

// SetVPNAutoSwitch turns automatic exit switching on or off (it is on unless turned off).
func (n *NodeApi) SetVPNAutoSwitch(on bool) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node is not running")
	}
	settingsMu.Lock()
	err := saveSettingsLocked(n.node.Cfg, func(p *persistedSettings) { p.VPNNoAutoSwitch = !on })
	if err == nil {
		n.noAutoSwitch = !on
	}
	settingsMu.Unlock()
	return err
}

// avoidedLocked lists the exits not to choose now. n.vpn.mu must be held.
func (n *NodeApi) avoidedLocked(now time.Time) map[string]bool {
	out := map[string]bool{}
	for id, until := range n.vpn.avoid {
		if now.Before(until) {
			out[id] = true
		} else {
			delete(n.vpn.avoid, id)
		}
	}
	return out
}

// pickExit finds an exit that is connected or reachable and says it serves us right now, best
// rated first, skipping avoid. Exits that fail the check are added to avoid.
func (n *NodeApi) pickExit(ctx context.Context, avoid map[string]bool) (string, error) {
	m, err := n.market()
	if err != nil {
		return "", err
	}
	if avoid == nil {
		avoid = map[string]bool{}
	}
	tries := 0
	for _, e := range m.Directory.List() {
		id := e.Advert.PeerID
		if avoid[id] || e.Rating.ConsecutiveFails >= 3 || (e.Rating.DomainsTried > 0 && e.Rating.DomainsOK == 0) {
			continue
		}
		pid, err := n.relayPeerID(id)
		if err != nil || pid == n.node.ID {
			continue
		}
		if tries++; tries > pickTries || ctx.Err() != nil {
			break
		}
		if err := n.ensureConnected(pid); err != nil {
			avoid[id] = true
			continue
		}
		qctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		info, err := exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(n.node.Swarm, pid)).QueryAccount(qctx)
		cancel()
		if state, _ := classifyExit(info, err); state != ExitOK {
			slog.Info("exit skipped", "exit", id[:12], "state", state)
			avoid[id] = true
			continue
		}
		return id, nil
	}
	return "", errors.New("no working exit found")
}

// runFailover watches the exit in use until ctx ends.
func (n *NodeApi) runFailover(ctx context.Context) {
	lost := 0
	var lastRefresh, lastRevive, lastStuckLog time.Time
	stuckWhy := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(failoverEvery):
		}
		n.vpn.mu.Lock()
		srv, relay, state, since := n.vpn.server, n.vpn.relayPeer, n.vpn.exitState, n.vpn.usingSince
		n.vpn.mu.Unlock()
		if srv == nil {
			return
		}
		if n.relayConnected(relay) {
			lost = 0
		} else {
			lost++
		}
		if !n.autoSwitchOn() {
			continue
		}
		now := time.Now()
		reason := exitFailure(exitCheck{Lost: lost, State: state, Health: srv.Health(), Using: now.Sub(since)}, now)
		if reason == "" {
			n.setSearching(false)
			stuckWhy = ""
			continue
		}
		n.setSearching(true)
		n.vpn.mu.Lock()
		if n.vpn.avoid == nil {
			n.vpn.avoid = map[string]time.Time{}
		}
		n.vpn.avoid[relay] = now.Add(failoverAvoidFor)
		avoid := n.avoidedLocked(now)
		n.vpn.mu.Unlock()

		next, err := n.pickExit(ctx, avoid)
		if err != nil && time.Since(lastRefresh) > refreshAtMostAt {
			lastRefresh = time.Now()
			_, _ = n.RefreshExitNodes() // look for exits we have not seen yet
			next, err = n.pickExit(ctx, avoid)
		}
		n.vpn.mu.Lock()
		for id := range avoid { // remember the ones that failed the check as well
			if _, ok := n.vpn.avoid[id]; !ok {
				n.vpn.avoid[id] = now.Add(failoverAvoidFor)
			}
		}
		n.vpn.mu.Unlock()
		if err != nil {
			// nothing else serves us: the exit in use may well come back (its link dropped, it
			// was busy for a moment), and nothing else would dial it again while it is avoided
			if time.Since(lastRevive) >= reviveEvery {
				lastRevive = time.Now()
				if n.reviveExit(ctx, relay) {
					lost = 0
					stuckWhy = ""
					continue
				}
			}
			if reason != stuckWhy || time.Since(lastStuckLog) >= stuckLogEvery {
				stuckWhy, lastStuckLog = reason, time.Now()
				slog.Warn("exit is not working and no other exit serves us yet", "exit", relay[:min(12, len(relay))], "why", reason)
			}
			continue
		}
		if n.switchExit(relay, next, reason) {
			lost = 0
		}
	}
}

// reviveExit connects again to the exit in use and asks it whether it serves us; when it does, the
// proxy stays on it and it is no longer avoided.
func (n *NodeApi) reviveExit(ctx context.Context, relay string) bool {
	pid, err := n.relayPeerID(relay)
	if err != nil || n.node == nil || n.node.Swarm == nil {
		return false
	}
	if err := n.ensureConnected(pid); err != nil || ctx.Err() != nil {
		return false
	}
	qctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	info, err := exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(n.node.Swarm, pid)).QueryAccount(qctx)
	cancel()
	n.noteExitState(relay, info, err)
	if state, _ := classifyExit(info, err); state != ExitOK {
		return false
	}
	n.vpn.mu.Lock()
	delete(n.vpn.avoid, relay)
	n.vpn.mu.Unlock()
	n.setSearching(false)
	slog.Info("the exit in use answers again", "exit", relay[:min(12, len(relay))])
	return true
}

// avoidNow lists the exits that failed recently.
func (n *NodeApi) avoidNow() map[string]bool {
	n.vpn.mu.Lock()
	defer n.vpn.mu.Unlock()
	return n.avoidedLocked(time.Now())
}

func (n *NodeApi) setSearching(on bool) {
	n.vpn.mu.Lock()
	changed := n.vpn.searching != on && n.vpn.server != nil
	n.vpn.searching = on
	n.vpn.mu.Unlock()
	if changed && on {
		n.emit("vpn:searching", map[string]any{})
	}
}

// switchExit moves the running proxy from exit from to exit to. It does nothing when the proxy
// was stopped or moved meanwhile.
func (n *NodeApi) switchExit(from, to, reason string) bool {
	id, err := n.relayPeerID(to)
	if err != nil || n.node == nil {
		return false
	}
	n.vpn.mu.Lock()
	if n.vpn.server == nil || n.vpn.relayPeer != from {
		n.vpn.mu.Unlock()
		return false
	}
	n.endSessionLocked("switched")
	n.vpn.server.SetDialer(exit_gateway.NewP2PDialer(exit_gateway.SwarmOpener(n.node.Swarm, id)))
	n.vpn.relayPeer = to
	n.vpn.exitState, n.vpn.exitNotice = ExitChecking, ""
	n.vpn.usingSince = time.Now()
	n.vpn.searching = false
	n.vpn.switches++
	n.vpn.lastSwitch = VPNSwitch{From: from, To: to, Reason: reason, At: time.Now().Unix()}
	n.beginSessionLocked(to)
	n.vpn.mu.Unlock()

	// the payment guard follows the new exit
	n.stopPayGuard()
	go n.startPayGuard()
	slog.Warn("switched to another exit", "from", from[:min(12, len(from))], "to", to[:min(12, len(to))], "why", reason)
	n.emit("vpn:switched", map[string]any{"from": from, "to": to, "reason": reason})
	return true
}
