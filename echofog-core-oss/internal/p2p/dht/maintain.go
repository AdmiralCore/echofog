package dht

import (
	"context"
	"crypto/rand"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/p2p/swarm"
)

const (
	// bucketRefreshAge: a bucket no lookup went into for this long gets one of a random ID in
	// it, so the table keeps up with nodes coming and going in every part of the ID space.
	// Jittered per check between 10 and 15 minutes.
	bucketRefreshMin = 10 * time.Minute
	bucketRefreshMax = 15 * time.Minute

	maintainEvery  = time.Minute
	saveEvery      = 5 * time.Minute
	selfLookupWait = 30 * time.Second
	refreshWait    = 20 * time.Second
	// maxRefreshesPerTick spreads the refreshes of a table gone stale (a laptop waking up) over
	// several ticks instead of dialing peers for all buckets at once.
	maxRefreshesPerTick = 4
)

func (d *EchoFogDHT) maintain(ctx context.Context) {
	defer close(d.done)
	tick := time.NewTicker(maintainEvery)
	defer tick.Stop()
	save := time.NewTicker(saveEvery)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.joined:
			d.SelfLookup(ctx)
		case <-tick.C:
			d.RefreshStaleBuckets(ctx)
		case <-save.C:
			if err := d.routingTable.Save(); err != nil {
				slog.Debug("DHT: saving the routing table failed", "err", err)
			}
		}
	}
}

// SelfLookup looks up this node's own ID: it finds the peers closest to us, and tells them
// about us, which is how a node that just joined becomes known to its neighbourhood.
func (d *EchoFogDHT) SelfLookup(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, selfLookupWait)
	defer cancel()
	res := d.newLookup(d.swarm.GetSelfID(), nil, nil).run(ctx)
	slog.Debug("DHT: self lookup done", "closest", len(res.closest), "table", d.routingTable.Len())
	return len(res.closest)
}

// RefreshStaleBuckets runs a lookup of a random ID in buckets no lookup went into lately.
// Returns how many it refreshed.
func (d *EchoFogDHT) RefreshStaleBuckets(ctx context.Context) int {
	age := bucketRefreshMin + time.Duration(mrand.Int64N(int64(bucketRefreshMax-bucketRefreshMin)))
	return d.refreshBuckets(ctx, age, maxRefreshesPerTick)
}

// RefreshAllBuckets refreshes every bucket that holds peers, however recently a lookup went into
// it (tools and measurements; the maintainer refreshes stale ones only).
func (d *EchoFogDHT) RefreshAllBuckets(ctx context.Context) int {
	return d.refreshBuckets(ctx, 0, IDLengthBits)
}

func (d *EchoFogDHT) refreshBuckets(ctx context.Context, age time.Duration, limit int) int {
	if len(d.swarm.GetAllPeers()) == 0 {
		return 0 // nobody to ask; the rejoin lookup covers it once a peer is back
	}
	stale := d.routingTable.staleBuckets(time.Now(), age)
	if len(stale) > limit {
		mrand.Shuffle(len(stale), func(i, j int) { stale[i], stale[j] = stale[j], stale[i] })
		stale = stale[:limit]
	}
	// the buckets are refreshed at once: each lookup is bounded on its own, and one that runs into
	// peers that left must not hold up the others (or the next rejoin lookup)
	var wg sync.WaitGroup
	for _, idx := range stale {
		var random [32]byte
		_, _ = rand.Read(random[:])
		target := randomIDInBucket(d.swarm.GetSelfID(), idx, random)
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, refreshWait)
			defer cancel()
			d.newLookup(target, nil, nil).run(lctx)
		}()
	}
	wg.Wait()
	return len(stale)
}

// dialCandidates offers the routing table's peers, most recently seen first, to the swarm's
// maintainer for when the node runs short of peers.
func (d *EchoFogDHT) dialCandidates(max int) []swarm.DialCandidate {
	var out []swarm.DialCandidate
	for _, p := range d.routingTable.allPeers() {
		if len(out) >= max {
			break
		}
		if p.Addr == "" || d.swarm.ThisIsActivePeer(p.ID) || d.isUnreachable(p.ID) {
			continue
		}
		out = append(out, swarm.DialCandidate{ID: p.ID, Addr: p.Addr})
	}
	return out
}
