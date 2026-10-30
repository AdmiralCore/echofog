package tonwallet

import (
	"errors"
	"net/http"
	"sync/atomic"
)

// The wallet's way out. Its servers (liteservers) are reached over plain TCP that cannot be sent
// through a proxy, so anonymous mode can keep the wallet offline (SetNetworkGate); the config and
// the rate, fetched over HTTP, take the client SetHTTPClient gives (through Tor, say).

var netGate atomic.Pointer[func() error]

// SetNetworkGate makes every connection to the TON servers ask gate first (nil: always allowed).
func SetNetworkGate(gate func() error) {
	if gate == nil {
		netGate.Store(nil)
		return
	}
	netGate.Store(&gate)
}

func gateErr() error {
	if g := netGate.Load(); g != nil {
		return (*g)()
	}
	return nil
}

// errNoHTTP: anonymous mode allows no request now (no proxy and no VPN to send it through).
var errNoHTTP = errors.New("anonymous mode: no way to make this request without showing the address")

var httpClient atomic.Pointer[func() *http.Client]

// SetHTTPClient sets where the wallet's HTTP requests go (nil: the default client). The function
// may return nil: no request then.
func SetHTTPClient(fn func() *http.Client) {
	if fn == nil {
		httpClient.Store(nil)
		return
	}
	httpClient.Store(&fn)
}

// client is the HTTP client for a request now; nil means none may be made.
func client() *http.Client {
	if f := httpClient.Load(); f != nil {
		return (*f)()
	}
	return http.DefaultClient
}
