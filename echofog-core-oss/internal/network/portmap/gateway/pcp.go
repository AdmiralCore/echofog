package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// PCP (Port Control Protocol, RFC 6887) is what newer routers and carrier-grade NATs speak in
// place of NAT-PMP, on the same port. A router that only knows NAT-PMP answers a PCP request with
// "unsupported version", and discovery falls back to NAT-PMP. Only what a peer needs is here: an
// ANNOUNCE to find the server, and MAP to open (and renew) a port; mappings are not deleted
// explicitly, they run out with their lifetime like the NAT-PMP ones.

const (
	pcpPort     = 5351
	pcpVersion  = 2
	opAnnounce  = 0
	opMap       = 1
	pcpResponse = 0x80 // R bit of the opcode byte

	pcpHeaderLen = 24
	pcpMapLen    = 36

	pcpSuccess         = 0
	pcpUnsuppVersion   = 1
	pcpCannotProvide   = 8 // e.g. the suggested external port is taken
	pcpAddressMismatch = 12
)

var _ NAT = (*pcpNAT)(nil)

// pcpResultText names the result codes of RFC 6887 section 7.4.
var pcpResultText = map[byte]string{
	1: "unsupported version", 2: "not authorized", 3: "malformed request", 4: "unsupported opcode",
	5: "unsupported option", 6: "malformed option", 7: "network failure", 8: "no resources",
	9: "unsupported protocol", 10: "user exceeded quota", 11: "cannot provide external",
	12: "address mismatch", 13: "excessive remote peers",
}

type pcpError struct{ code byte }

func (e pcpError) Error() string {
	if s, ok := pcpResultText[e.code]; ok {
		return "pcp: " + s
	}
	return fmt.Sprintf("pcp: result %d", e.code)
}

type pcpNAT struct {
	server   *net.UDPAddr // the gateway's PCP port
	gateway  net.IP
	internal net.IP // our address on the gateway's network, sent in every request

	mu    sync.Mutex
	ports map[pcpKey]pcpMapping // live mappings, renewed with the same nonce
	ext   net.IP                // external address from the last mapping (PCP has no query for it)
}

type pcpKey struct {
	proto byte
	port  int
}

type pcpMapping struct {
	nonce [12]byte
	ext   int
}

func discoverPCP(ctx context.Context) (NAT, error) {
	gw, err := getDefaultGateway()
	if err != nil {
		return nil, err
	}
	return discoverPCPAt(ctx, &net.UDPAddr{IP: gw, Port: pcpPort})
}

func discoverPCPAt(ctx context.Context, server *net.UDPAddr) (*pcpNAT, error) {
	local, err := localAddrTowards(server)
	if err != nil {
		return nil, err
	}
	n := &pcpNAT{server: server, gateway: server.IP, internal: local, ports: map[pcpKey]pcpMapping{}}
	if _, err := n.exchange(ctx, n.request(opAnnounce, 0, nil)); err != nil {
		return nil, err
	}
	return n, nil
}

// localAddrTowards is the address the system would use to reach the server.
func localAddrTowards(server *net.UDPAddr) (net.IP, error) {
	c, err := net.DialUDP("udp", nil, server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}

// request builds a PCP request: the common header and, for MAP, its opcode data.
func (n *pcpNAT) request(op byte, lifetime uint32, mapData []byte) []byte {
	b := make([]byte, pcpHeaderLen, pcpHeaderLen+len(mapData))
	b[0], b[1] = pcpVersion, op
	binary.BigEndian.PutUint32(b[4:8], lifetime)
	copy(b[8:24], n.internal.To16()) // an IPv4 address goes in IPv4-mapped form
	return append(b, mapData...)
}

func mapData(nonce [12]byte, proto byte, internalPort, suggestedExt int) []byte {
	d := make([]byte, pcpMapLen)
	copy(d[0:12], nonce[:])
	d[12] = proto
	binary.BigEndian.PutUint16(d[16:18], uint16(internalPort))
	binary.BigEndian.PutUint16(d[18:20], uint16(suggestedExt))
	copy(d[20:36], net.IPv6zero) // no suggested external address
	return d
}

// exchange sends req and waits for the matching response, resending with the backoff NAT-PMP
// uses (the RFC's own starts at 3 s, too slow for a start-up check).
func (n *pcpNAT) exchange(ctx context.Context, req []byte) ([]byte, error) {
	c, err := net.DialUDP("udp", nil, n.server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	buf := make([]byte, 1100) // the largest PCP message
	wait := 250 * time.Millisecond
	for try := 0; try < 4; try++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := c.Write(req); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = c.SetReadDeadline(deadline)
		for {
			m, err := c.Read(buf)
			if err != nil {
				break // timed out: send again
			}
			resp := buf[:m]
			if m < 4 || resp[1] != req[1]|pcpResponse {
				continue // not an answer to this request
			}
			if resp[0] != pcpVersion {
				// a NAT-PMP router answers PCP with version 0 and "unsupported version"
				return nil, pcpError{pcpUnsuppVersion}
			}
			if m < pcpHeaderLen {
				continue
			}
			if code := resp[3]; code != pcpSuccess {
				return nil, pcpError{code}
			}
			return append([]byte(nil), resp...), nil
		}
		wait *= 2
	}
	return nil, errors.New("pcp: no answer from the gateway")
}

func pcpProto(protocol string) (byte, error) {
	switch protocol {
	case "tcp":
		return 6, nil
	case "udp":
		return 17, nil
	}
	return 0, fmt.Errorf("pcp: unknown protocol %q", protocol)
}

// AddPortMapping asks for internalPort to be reachable from outside, for timeout. A renewal
// reuses the mapping's nonce and asks for the external port it already has.
func (n *pcpNAT) AddPortMapping(ctx context.Context, protocol string, internalPort int, _ string, timeout time.Duration) (int, error) {
	proto, err := pcpProto(protocol)
	if err != nil {
		return 0, err
	}
	key := pcpKey{proto, internalPort}
	n.mu.Lock()
	m, known := n.ports[key]
	n.mu.Unlock()
	if !known {
		if _, err := rand.Read(m.nonce[:]); err != nil {
			return 0, err
		}
		m.ext = internalPort // ask for the same port outside; the server picks another if taken
	}
	lifetime := uint32(timeout / time.Second)
	resp, err := n.exchange(ctx, n.request(opMap, lifetime, mapData(m.nonce, proto, internalPort, m.ext)))
	var pe pcpError
	if errors.As(err, &pe) && pe.code == pcpCannotProvide && m.ext != 0 {
		// the suggested port is taken: let the server choose
		resp, err = n.exchange(ctx, n.request(opMap, lifetime, mapData(m.nonce, proto, internalPort, 0)))
	}
	if err != nil {
		return 0, err
	}
	if len(resp) < pcpHeaderLen+pcpMapLen {
		return 0, errors.New("pcp: short MAP response")
	}
	d := resp[pcpHeaderLen:]
	if [12]byte(d[0:12]) != m.nonce || d[12] != proto || int(binary.BigEndian.Uint16(d[16:18])) != internalPort {
		return 0, errors.New("pcp: MAP response for another mapping")
	}
	m.ext = int(binary.BigEndian.Uint16(d[18:20]))
	ext := net.IP(append([]byte(nil), d[20:36]...))
	if v4 := ext.To4(); v4 != nil {
		ext = v4
	}
	n.mu.Lock()
	n.ports[key] = m
	n.ext = ext
	n.mu.Unlock()
	return m.ext, nil
}

// DeletePortMapping forgets the mapping; the gateway drops it when its lifetime runs out.
func (n *pcpNAT) DeletePortMapping(_ context.Context, protocol string, internalPort int) error {
	proto, err := pcpProto(protocol)
	if err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.ports, pcpKey{proto, internalPort})
	n.mu.Unlock()
	return nil
}

// GetExternalAddress is the address the last mapping was given; PCP has no request for it alone.
func (n *pcpNAT) GetExternalAddress() (net.IP, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ext == nil || n.ext.IsUnspecified() {
		return nil, errors.New("pcp: external address known once a port is mapped")
	}
	return n.ext, nil
}

func (n *pcpNAT) GetDeviceAddress() (net.IP, error)   { return n.gateway, nil }
func (n *pcpNAT) GetInternalAddress() (net.IP, error) { return n.internal, nil }
func (n *pcpNAT) Type() string                        { return "PCP" }
