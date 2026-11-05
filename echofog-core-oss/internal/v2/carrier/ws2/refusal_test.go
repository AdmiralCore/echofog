package ws2

import (
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const fixedDelay = 300 * time.Millisecond

// refusalReasons are the ways a stranger can end up refused; every one of them must come out the
// same (AD20, wire.md §4.4): close code 1008 after the one delay of the listener.
func refusalReasons() map[string]func(*websocket.Conn) {
	return map[string]func(*websocket.Conn){
		"garbage": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte("GET / HTTP/1.1\r\n\r\n"))
		},
		"text message": func(ws *websocket.Conn) { _ = ws.WriteMessage(websocket.TextMessage, []byte("hello")) },
		"oversized message": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, make([]byte, MaxMessage+1))
		},
		"not a handshake record": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{0x17, 3, 3, 0, 5, 1, 2, 3, 4, 5})
		},
		"huge record": func(ws *websocket.Conn) {
			_ = ws.WriteMessage(websocket.BinaryMessage, []byte{0x16, 3, 1, 0xff, 0xff, 1})
		},
	}
}

func checkRefusal(t *testing.T, name string, code int, d time.Duration) {
	t.Helper()
	if code != websocket.ClosePolicyViolation {
		t.Errorf("%s: close code %d, want 1008", name, code)
	}
	if d < fixedDelay-20*time.Millisecond || d > 3*time.Second {
		t.Errorf("%s: closed after %v, want the listener's delay (%v): not every refusal goes through one function", name, d, fixedDelay)
	}
}

// TestEveryRefusalGoesThroughOneFunction: a table of reasons, one after another and N+1 at once
// (AD20): the observable outcome (code, delay) is the same for all of them.
func TestEveryRefusalGoesThroughOneFunction(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Delay = func() time.Duration { return fixedDelay } })
	t.Run("sequential", func(t *testing.T) {
		for name, f := range refusalReasons() {
			code, d := strangerOutcome(t, r, f)
			checkRefusal(t, name, code, d)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 3; i++ { // N+1 strangers, every reason several times
			for name, f := range refusalReasons() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					code, d := strangerOutcome(t, r, f)
					checkRefusal(t, name, code, d)
				}()
			}
		}
		wg.Wait()
	})
}

// TestOverPreTokenLimitUsesTheSameDelay: the connection over the pre-token limit waits the same.
func TestOverPreTokenLimitUsesTheSameDelay(t *testing.T) {
	r := newRig(t, func(c *Config) {
		c.MaxPreToken = 1
		c.Delay = func() time.Duration { return fixedDelay }
	})
	hold, _, err := r.rawWS(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	time.Sleep(300 * time.Millisecond)
	code, d := strangerOutcome(t, r, func(ws *websocket.Conn) { _ = ws.WriteMessage(websocket.BinaryMessage, []byte("x")) })
	checkRefusal(t, "over the pre-token limit", code, d)
}

// TestMaxConnsServesTheSite: over the WebSocket limit the next upgrade gets the ordinary site,
// and a slot is free again once a connection ends.
func TestMaxConnsServesTheSite(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxConns = 1 })
	hold, _, err := r.rawWS(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if ws, resp, err := r.rawWS(secretPath); err == nil {
		ws.Close()
		t.Fatal("a second WebSocket was upgraded over MaxConns")
	} else if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("over MaxConns: %v %v", resp, err)
	}
	hold.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ws, _, err := r.rawWS(secretPath)
		if err == nil {
			ws.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot was not given back: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestClientAddrFromHeader: only a header with one valid address counts (F3): a list such as
// X-Forwarded-For, whose left part the client writes, is not trusted.
func TestClientAddrFromHeader(t *testing.T) {
	l := &Listener{cfg: Config{ClientIPHeader: "CF-Connecting-IP"}}
	for _, tc := range []struct{ header, want string }{
		{"203.0.113.7", "203.0.113.7"},
		{"2001:db8::1", "2001:db8::1"},
		{"", "192.0.2.1"},
		{"not an address", "192.0.2.1"},
		{"198.51.100.1, 203.0.113.7", "192.0.2.1"},
	} {
		r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
		r.RemoteAddr = "192.0.2.1:5555"
		if tc.header != "" {
			r.Header.Set("CF-Connecting-IP", tc.header)
		}
		if got := l.clientAddr(r).(*net.TCPAddr).IP.String(); got != tc.want {
			t.Errorf("header %q: %s, want %s", tc.header, got, tc.want)
		}
	}
}
