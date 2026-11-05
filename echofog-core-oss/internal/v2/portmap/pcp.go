package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"time"
)

// PCP (RFC 6887) and NAT-PMP (RFC 6886) share a port. Only what a node needs is here: PCP
// ANNOUNCE to find the server and MAP to open, renew and drop a port; NAT-PMP external address
// and MAP. Packets are built by hand; the parsers are bounded and ignore what is not an answer.

const (
	pcpVersion    = 2
	pcpOpAnnounce = 0
	pcpOpMap      = 1
	pcpResponse   = 0x80

	pcpHeaderLen = 24
	pcpMapLen    = 36
	pcpNonceLen  = 12

	pcpUnsuppVersion = 1
	pcpNoResources   = 8
	pcpCannotProvide = 11
)

var pcpResultText = map[byte]string{
	1: "unsupported version", 2: "not authorized", 3: "malformed request", 4: "unsupported opcode",
	5: "unsupported option", 6: "malformed option", 7: "network failure", 8: "no resources",
	9: "unsupported protocol", 10: "user exceeded quota", 11: "cannot provide external",
	12: "address mismatch", 13: "excessive remote peers",
}

type pcpError struct{ code byte }

func (e pcpError) Error() string {
	if s, ok := pcpResultText[e.code]; ok {
		return "portmap: pcp: " + s
	}
	return fmt.Sprintf("portmap: pcp: result %d", e.code)
}

type pcpClient struct {
	m      *Manager
	server netip.AddrPort
	local  netip.Addr
	nonces map[key][pcpNonceLen]byte // only the worker touches it
}

func (m *Manager) serverPort() uint16 { return m.cfg.Port }

func (m *Manager) discoverPCP(ctx context.Context, gw, local netip.Addr) (gateway, error) {
	c := &pcpClient{m: m, server: netip.AddrPortFrom(gw, m.serverPort()), local: local, nonces: map[key][pcpNonceLen]byte{}}
	req := c.header(pcpOpAnnounce, 0)
	err := m.exchange(ctx, c.server, req, func(p []byte) (bool, error) {
		if unsupportedByNATPMPRouter(p) {
			return true, errUnsupported
		}
		r, ok, err := parsePCPHeader(p, pcpOpAnnounce)
		if !ok {
			return false, nil
		}
		_ = r
		return true, err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// unsupportedByNATPMPRouter: a router that only speaks NAT-PMP answers a PCP request in the
// NAT-PMP layout (version 0, result 1 in the 16-bit field).
func unsupportedByNATPMPRouter(p []byte) bool {
	return len(p) >= 8 && p[0] == 0 && p[1]&pcpResponse != 0 && binary.BigEndian.Uint16(p[2:4]) == 1
}

func (c *pcpClient) name() string { return "PCP" }

func (c *pcpClient) header(op byte, lifetime uint32) []byte {
	b := make([]byte, pcpHeaderLen)
	b[0], b[1] = pcpVersion, op
	binary.BigEndian.PutUint32(b[4:8], lifetime)
	a := c.local.As16()
	copy(b[8:24], a[:])
	return b
}

func pcpProto(p Proto) (byte, error) {
	switch p {
	case TCP:
		return 6, nil
	case UDP:
		return 17, nil
	}
	return 0, ErrBadPort
}

func (c *pcpClient) mapRequest(nonce [pcpNonceLen]byte, proto byte, internal, suggested uint16, lifetime uint32) []byte {
	b := c.header(pcpOpMap, lifetime)
	d := make([]byte, pcpMapLen) // the suggested external address stays all zeros
	copy(d[0:12], nonce[:])
	d[12] = proto
	binary.BigEndian.PutUint16(d[16:18], internal)
	binary.BigEndian.PutUint16(d[18:20], suggested)
	return append(b, d...)
}

func (c *pcpClient) mapPort(ctx context.Context, p Proto, internal, suggested uint16, lifetime time.Duration) (mapResult, error) {
	proto, err := pcpProto(p)
	if err != nil {
		return mapResult{}, err
	}
	k := key{p, internal}
	nonce, known := c.nonces[k]
	if !known {
		if _, err := io.ReadFull(c.m.cfg.Nonce, nonce[:]); err != nil {
			return mapResult{}, err
		}
	}
	secs := uint32(lifetime / time.Second)
	res, err := c.mapExchange(ctx, nonce, proto, internal, suggested, secs)
	if pe, ok := err.(pcpError); ok && (pe.code == pcpNoResources || pe.code == pcpCannotProvide) && suggested != 0 {
		res, err = c.mapExchange(ctx, nonce, proto, internal, 0, secs) // let the router choose
	}
	if err != nil {
		return mapResult{}, err
	}
	c.nonces[k] = nonce
	return res, nil
}

func (c *pcpClient) mapExchange(ctx context.Context, nonce [pcpNonceLen]byte, proto byte, internal, suggested uint16, secs uint32) (mapResult, error) {
	var res mapResult
	req := c.mapRequest(nonce, proto, internal, suggested, secs)
	err := c.m.exchange(ctx, c.server, req, func(p []byte) (bool, error) {
		hdr, ok, err := parsePCPHeader(p, pcpOpMap)
		if !ok {
			return false, nil
		}
		if err != nil {
			return true, err
		}
		r, ok := parsePCPMap(p, hdr, nonce, proto, internal)
		if !ok {
			return false, nil // an answer to another mapping
		}
		res = r
		return true, nil
	})
	return res, err
}

func (c *pcpClient) unmap(ctx context.Context, p Proto, internal, ext uint16) error {
	proto, err := pcpProto(p)
	if err != nil {
		return err
	}
	k := key{p, internal}
	nonce, known := c.nonces[k]
	if !known {
		return nil
	}
	delete(c.nonces, k)
	return c.m.send(ctx, c.server, c.mapRequest(nonce, proto, internal, ext, 0))
}

type pcpHeader struct {
	lifetime uint32
}

// parsePCPHeader checks the common response header for the opcode. ok is false for anything
// that is not a PCP answer to this kind of request; err is the router's refusal.
func parsePCPHeader(p []byte, op byte) (h pcpHeader, ok bool, err error) {
	if len(p) < pcpHeaderLen || p[0] != pcpVersion || p[1] != op|pcpResponse {
		return h, false, nil
	}
	if code := p[3]; code != 0 {
		return h, true, pcpError{code}
	}
	return pcpHeader{lifetime: binary.BigEndian.Uint32(p[4:8])}, true, nil
}

// parsePCPMap reads the MAP opcode data of an answer; ok is false if it is not the answer to
// the mapping (nonce, protocol and internal port) that was asked for.
func parsePCPMap(p []byte, h pcpHeader, nonce [pcpNonceLen]byte, proto byte, internal uint16) (mapResult, bool) {
	if len(p) < pcpHeaderLen+pcpMapLen {
		return mapResult{}, false
	}
	d := p[pcpHeaderLen:]
	if [pcpNonceLen]byte(d[0:12]) != nonce || d[12] != proto || binary.BigEndian.Uint16(d[16:18]) != internal {
		return mapResult{}, false
	}
	addr := netip.AddrFrom16([16]byte(d[20:36])).Unmap()
	return mapResult{extPort: binary.BigEndian.Uint16(d[18:20]), addr: addr, lifetime: time.Duration(h.lifetime) * time.Second}, true
}
