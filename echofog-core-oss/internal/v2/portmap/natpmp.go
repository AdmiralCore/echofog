package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

const (
	pmpExtAddrLen = 12
	pmpMapLen     = 16
	pmpOpExt      = 0
	pmpOpUDP      = 1
	pmpOpTCP      = 2
)

var pmpResultText = map[uint16]string{
	1: "unsupported version", 2: "not authorized", 3: "network failure", 4: "out of resources", 5: "unsupported opcode",
}

type pmpError struct{ code uint16 }

func (e pmpError) Error() string {
	if s, ok := pmpResultText[e.code]; ok {
		return "portmap: nat-pmp: " + s
	}
	return fmt.Sprintf("portmap: nat-pmp: result %d", e.code)
}

type natpmpClient struct {
	m      *Manager
	server netip.AddrPort
}

func (m *Manager) discoverNATPMP(ctx context.Context, gw netip.Addr) (gateway, error) {
	c := &natpmpClient{m: m, server: netip.AddrPortFrom(gw, m.serverPort())}
	if _, err := c.externalAddr(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *natpmpClient) name() string { return "NAT-PMP" }

func (c *natpmpClient) externalAddr(ctx context.Context) (netip.Addr, error) {
	var addr netip.Addr
	err := c.m.exchange(ctx, c.server, []byte{0, pmpOpExt}, func(p []byte) (bool, error) {
		a, ok, err := parsePMPExternal(p)
		if !ok {
			return false, nil
		}
		addr = a
		return true, err
	})
	return addr, err
}

func pmpOp(p Proto) (byte, error) {
	switch p {
	case UDP:
		return pmpOpUDP, nil
	case TCP:
		return pmpOpTCP, nil
	}
	return 0, ErrBadPort
}

func pmpMapRequest(op byte, internal, suggested uint16, secs uint32) []byte {
	b := make([]byte, pmpMapLen-4)
	b[1] = op
	binary.BigEndian.PutUint16(b[4:6], internal)
	binary.BigEndian.PutUint16(b[6:8], suggested)
	binary.BigEndian.PutUint32(b[8:12], secs)
	return b
}

func (c *natpmpClient) mapPort(ctx context.Context, p Proto, internal, suggested uint16, lifetime time.Duration) (mapResult, error) {
	op, err := pmpOp(p)
	if err != nil {
		return mapResult{}, err
	}
	addr, err := c.externalAddr(ctx)
	if err != nil {
		return mapResult{}, err
	}
	res := mapResult{addr: addr}
	req := pmpMapRequest(op, internal, suggested, uint32(lifetime/time.Second))
	err = c.m.exchange(ctx, c.server, req, func(pk []byte) (bool, error) {
		port, life, ok, err := parsePMPMap(pk, op, internal)
		if !ok {
			return false, nil
		}
		res.extPort, res.lifetime = port, life
		return true, err
	})
	return res, err
}

func (c *natpmpClient) unmap(ctx context.Context, p Proto, internal, _ uint16) error {
	op, err := pmpOp(p)
	if err != nil {
		return err
	}
	return c.m.send(ctx, c.server, pmpMapRequest(op, internal, 0, 0))
}

// parsePMPExternal reads the answer to the external address request.
func parsePMPExternal(p []byte) (addr netip.Addr, ok bool, err error) {
	if len(p) != pmpExtAddrLen || p[0] != 0 || p[1] != 128+pmpOpExt {
		return addr, false, nil
	}
	if code := binary.BigEndian.Uint16(p[2:4]); code != 0 {
		return addr, true, pmpError{code}
	}
	return netip.AddrFrom4([4]byte(p[8:12])), true, nil
}

// parsePMPMap reads the answer to a MAP request for the internal port.
func parsePMPMap(p []byte, op byte, internal uint16) (ext uint16, life time.Duration, ok bool, err error) {
	if len(p) != pmpMapLen || p[0] != 0 || p[1] != 128+op || binary.BigEndian.Uint16(p[8:10]) != internal {
		return 0, 0, false, nil
	}
	if code := binary.BigEndian.Uint16(p[2:4]); code != 0 {
		return 0, 0, true, pmpError{code}
	}
	return binary.BigEndian.Uint16(p[10:12]), time.Duration(binary.BigEndian.Uint32(p[12:16])) * time.Second, true, nil
}
