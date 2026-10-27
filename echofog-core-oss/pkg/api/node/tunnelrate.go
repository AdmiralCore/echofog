package node

import (
	"time"
)

// The speed an exit is rated with comes from what its tunnel really delivers: the market's
// background checks send no download burst (it would be a noise spike over the mesh link a censor
// flags), so while the proxy runs, the watcher reads the download counter and passes busy stretches
// on to the exit's rating.

const (
	rateBusyBytes = 1 << 20          // a round that downloads less says how little was asked, not how fast the exit is
	rateSettle    = 30 * time.Second // after a switch, connections still open on the previous exit count in the proxy too
)

// tunnelRate turns the proxy's download counter into the rate the exit in use delivered.
type tunnelRate struct {
	relay string
	rx    uint64
	at    time.Time
}

// sample takes the counter at now and returns the rate in kbps since the last sample, when that
// stretch was busy enough to say something about the exit and the exit has been in use since
// before rateSettle.
func (t *tunnelRate) sample(relay string, rx uint64, since, now time.Time) (int, bool) {
	prev := *t
	*t = tunnelRate{relay: relay, rx: rx, at: now}
	if prev.at.IsZero() || prev.relay != relay || rx < prev.rx || prev.at.Sub(since) < rateSettle {
		return 0, false
	}
	elapsed := now.Sub(prev.at)
	if rx-prev.rx < rateBusyBytes || elapsed <= 0 {
		return 0, false
	}
	return int(float64(rx-prev.rx) * 8 / 1000 / elapsed.Seconds()), true
}

// noteTunnelRate passes a busy stretch of the tunnel through relay on to that exit's rating.
func (n *NodeApi) noteTunnelRate(t *tunnelRate, relay string, rx uint64, since time.Time) {
	kbps, ok := t.sample(relay, rx, since, time.Now())
	if !ok {
		return
	}
	m, err := n.market()
	if err != nil {
		return
	}
	if id, err := n.relayPeerID(relay); err == nil {
		_ = m.NoteThroughput(id, kbps)
	}
}
