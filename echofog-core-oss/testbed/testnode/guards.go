package main

import (
	"context"
	"crypto/rand"
	"net/http"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// guardRoutes: what the guard-mode scenario of the launchpad reads.
func (t *testNode) guardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/guards", t.guards)
	mux.HandleFunc("/lookup", t.lookup)
}

func (t *testNode) guards(w http.ResponseWriter, _ *http.Request) {
	type g struct {
		ID        string `json:"id"`
		Addr      string `json:"addr"`
		Connected bool   `json:"connected"`
	}
	out := []g{}
	for _, x := range t.n.Swarm.Guards() {
		out = append(out, g{x.ID.String(), x.Addr, x.Connected})
	}
	reply(w, map[string]any{"active": t.n.Swarm.GuardModeActive(), "guards": out, "peers": len(t.n.Swarm.GetAllPeers())}, nil)
}

// lookup runs one DHT peer lookup and says how long it took: of the peer in ?peer=, or of a random
// ID that nobody holds (the lookup then walks to the end).
func (t *testNode) lookup(w http.ResponseWriter, r *http.Request) {
	var target types.PeerID
	if s := r.URL.Query().Get("peer"); s != "" {
		id, err := types.PeerIDFromString(s)
		if err != nil {
			reply(w, nil, err)
			return
		}
		target = id
	} else {
		_, _ = rand.Read(target[:])
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	addr, err := t.n.DhtEngine.FindPeer(ctx, target)
	res := map[string]any{"ms": time.Since(start).Milliseconds(), "found": err == nil, "addr": addr}
	if err != nil {
		res["error"] = err.Error()
	}
	reply(w, res, nil)
}
