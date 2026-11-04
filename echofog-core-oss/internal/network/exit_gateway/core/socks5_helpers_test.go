package core

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// socksDial opens a CONNECT through the proxy without the test deadline socksConnect sets.
func socksDial(proxy, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		c.Close()
		return nil, err
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		c.Close()
		return nil, err
	}
	host, port, _ := net.SplitHostPort(target)
	var p uint16
	fmt.Sscan(port, &p)
	req := append([]byte{5, 1, 0, 1}, net.ParseIP(host).To4()...)
	req = binary.BigEndian.AppendUint16(req, p)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 10)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks reply %d", resp[1])
	}
	return c, nil
}
