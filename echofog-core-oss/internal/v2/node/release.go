package node

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/release"
)

const (
	// releasePollInterval is how often the DHT and the download page are asked for a newer manifest.
	releasePollInterval = 24 * time.Hour
	// releasePollFirst is the uniform delay [min, max) before the first poll, so a restart does not
	// look like a burst and the DHT has time to fill.
	releasePollFirstMin = 2 * time.Minute
	releasePollFirstMax = 10 * time.Minute
)

// Release returns the release manifest state of the node, or nil when the build has no release
// keys (Config.ReleaseKeys).
func (r *Runtime) Release() *release.Manager { return r.releaseM }

// newReleaseManager builds the manager from Config.ReleaseKeys. Nil without keys.
func (r *Runtime) newReleaseManager() (*release.Manager, error) {
	if len(r.cfg.ReleaseKeys) == 0 {
		return nil, nil
	}
	var kv KV
	if r.cfg.Store != nil {
		kv = r.cfg.Store.Store(release.Namespace)
	}
	return release.Open(release.Config{
		Keys:  r.cfg.ReleaseKeys,
		KV:    kv,
		Clock: r.clock,
		Own:   map[string]uint32{"proto": handshake.ProtoVer},
		OnEvent: func(e release.Event) {
			ev := e
			r.events.Emit(Event{
				Kind:    EventUpdateAvailable,
				Message: "a newer version of EchoFog is available",
				At:      e.At,
				Update:  &ev,
			})
		},
		Log: r.log,
	})
}

// admitVersion is the handshake version admission built from the manifest: a peer below
// min_supported gets REJECT{version}; the node's own version is never refused (never lock out).
func (r *Runtime) admitVersion() func(peerProto uint32) bool {
	if r.releaseM == nil {
		return nil
	}
	return r.releaseM.Admit("proto", handshake.ProtoVer)
}

// releaseCheck is the DHT server's release check: the explicit Config.ReleaseCheck, else the
// manager's.
func (r *Runtime) releaseCheck() func(raw []byte, now time.Time) error {
	if r.cfg.ReleaseCheck != nil {
		return r.cfg.ReleaseCheck
	}
	if r.releaseM != nil {
		return r.releaseM.Check
	}
	return nil
}

// pexRelease and pexOnRelease are the pex/2 hooks: hand the manifest out, apply the one an
// answerer sent. Both are nil without a manager.
func (r *Runtime) pexRelease() func() []byte {
	if r.releaseM == nil {
		return nil
	}
	return r.releaseM.Latest
}

func (r *Runtime) pexOnRelease() func([]byte) {
	if r.releaseM == nil {
		return nil
	}
	return func(raw []byte) {
		if _, err := r.releaseM.Offer(raw); err != nil {
			r.log.Debug("node: release manifest from pex not applied", "err", err)
		}
	}
}

// releaseLoop polls the DHT and the download page for a newer manifest. Failures are silent: the
// sources are optional and the next poll retries. The manifest only informs the user (R5).
func (r *Runtime) releaseLoop(ctx context.Context) {
	defer r.wg.Done()
	interval := r.cfg.ReleasePoll
	first := releasePollFirstMin + time.Duration(rand.Int64N(int64(releasePollFirstMax-releasePollFirstMin)))
	if interval <= 0 {
		interval = releasePollInterval
	} else {
		first = interval
	}
	for {
		if !r.sleep(ctx, first) {
			return
		}
		r.pollRelease(ctx)
		// ±20 % jitter (AD13).
		first = interval - interval/5 + time.Duration(rand.Int64N(int64(interval)*2/5+1))
	}
}

func (r *Runtime) sleep(ctx context.Context, d time.Duration) bool {
	fire := make(chan struct{}, 1)
	t := r.carrierClock.AfterFunc(d, func() {
		select {
		case fire <- struct{}{}:
		default:
		}
	})
	select {
	case <-fire:
		return true
	case <-ctx.Done():
		t.Stop()
		return false
	}
}

func (r *Runtime) pollRelease(ctx context.Context) {
	m := r.releaseM
	if m == nil {
		return
	}
	if cl := r.DHT(); cl != nil {
		pctx, cancel := context.WithTimeout(ctx, time.Minute)
		if _, err := m.PollDHT(pctx, cl); err != nil {
			r.log.Debug("node: release manifest from the DHT", "err", err)
		}
		cancel()
	}
	if u := r.cfg.ReleaseSiteURL; u != "" {
		pctx, cancel := context.WithTimeout(ctx, time.Minute)
		if _, err := m.FetchSite(pctx, r.http, u); err != nil {
			r.log.Debug("node: release manifest from the download page", "err", err)
		}
		cancel()
	}
}
