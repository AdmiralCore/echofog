package natcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun"
)

// DefaultSTUNServers are public STUN servers from unrelated operators. Several are queried
// at once so that one blocked or dead server (common in censored networks) does not blind us, and
// so that their answers can be compared. Operators can override the list in the configuration.
var DefaultSTUNServers = []string{
	"stun.l.google.com:19302",
	"stun.cloudflare.com:3478",
	"global.stun.twilio.com:3478",
	"stun.nextcloud.com:3478",
}

// ErrNoSTUNAnswer: no server answered (UDP blocked, or every server unreachable).
var ErrNoSTUNAnswer = errors.New("no STUN server answered")

// QuerySTUN sends a binding request to every server from conn (the very socket whose mapping we
// want to know) and collects the answers that arrive within timeout. Servers that do not answer
// are skipped; it fails only if none does.
//
// conn must be dedicated to this call while it runs: it sets read deadlines and reads replies.
func QuerySTUN(ctx context.Context, conn net.PacketConn, servers []string, timeout time.Duration) ([]Observation, error) {
	if len(servers) == 0 {
		return nil, errors.New("no STUN servers configured")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	type pending struct{ server string }
	sent := map[[stun.TransactionIDSize]byte]pending{}
	var mu sync.Mutex

	// resolve and send to all servers up front
	for _, s := range servers {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		ua, err := resolveUDP4(rctx, s)
		cancel()
		if err != nil {
			continue
		}
		msg, err := stun.Build(stun.TransactionID, stun.BindingRequest)
		if err != nil {
			continue
		}
		var id [stun.TransactionIDSize]byte
		copy(id[:], msg.TransactionID[:])
		mu.Lock()
		sent[id] = pending{server: s}
		mu.Unlock()
		if _, err := conn.WriteTo(msg.Raw, ua); err != nil {
			mu.Lock()
			delete(sent, id)
			mu.Unlock()
		}
	}
	if len(sent) == 0 {
		return nil, ErrNoSTUNAnswer
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	defer conn.SetReadDeadline(time.Time{})

	want := len(sent) // answered requests leave the map, so remember how many we are waiting for
	var out []Observation
	buf := make([]byte, 1500)
	for len(out) < want {
		if ctx.Err() != nil {
			break
		}
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break // deadline: take what we have
		}
		var m stun.Message
		m.Raw = append([]byte(nil), buf[:n]...)
		if m.Decode() != nil || m.Type != stun.BindingSuccess {
			continue
		}
		var id [stun.TransactionIDSize]byte
		copy(id[:], m.TransactionID[:])
		mu.Lock()
		p, ok := sent[id]
		delete(sent, id) // one answer per request: a spoofed duplicate is ignored
		mu.Unlock()
		if !ok {
			continue
		}
		var xa stun.XORMappedAddress
		if err := xa.GetFrom(&m); err != nil {
			var ma stun.MappedAddress // very old servers
			if ma.GetFrom(&m) != nil {
				continue
			}
			xa.IP, xa.Port = ma.IP, ma.Port
		}
		addr, ok := netip.AddrFromSlice(xa.IP)
		if !ok {
			continue
		}
		out = append(out, Observation{From: p.server, Addr: netip.AddrPortFrom(addr.Unmap(), uint16(xa.Port))})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w (is UDP blocked?)", ErrNoSTUNAnswer)
	}
	return out, nil
}

func resolveUDP4(ctx context.Context, hostport string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: %v", hostport, err)
	}
	p, err := net.LookupPort("udp", port)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ips[0].AsSlice(), Port: p}, nil
}

// LocalAddrs lists the addresses of our network interfaces (for Report.NoNAT).
func LocalAddrs() []netip.Addr {
	var out []netip.Addr
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP); ok && !ip.IsLoopback() {
					out = append(out, ip.Unmap())
				}
			}
		}
	}
	return out
}
