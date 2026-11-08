package node_test

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
)

// TestEgressLiveThroughTor goes out to the real network through a running Tor (T11.6a). It is off
// unless ECHOFOG_LIVE_TOR names the SOCKS port, e.g. ECHOFOG_LIVE_TOR=127.0.0.1:9050: the node's
// Host.HTTP() asks check.torproject.org, which answers whether the request came out of Tor.
func TestEgressLiveThroughTor(t *testing.T) {
	addr := os.Getenv("ECHOFOG_LIVE_TOR")
	if addr == "" {
		t.Skip("set ECHOFOG_LIVE_TOR=127.0.0.1:9050 to run against a real Tor")
	}
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) { c.Egress = &node.Egress{Proxy: addr, Tor: true} })
	resp, err := a.rt.HTTP().Get("https://check.torproject.org/api/ip")
	if err != nil {
		t.Fatalf("GET through Tor: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		IsTor bool `json:"IsTor"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("answer %q: %v", body, err)
	}
	if !out.IsTor {
		t.Fatalf("the request did not leave through Tor: %s", body)
	}
}
