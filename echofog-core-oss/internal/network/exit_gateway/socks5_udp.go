package exit_gateway

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/fakedns"
)

// UDPDialer is implemented by dialers that can relay UDP through the exit (P2PDialer does).
type UDPDialer interface {
	DialExitUDP(ctx context.Context) (*UDPSession, error)
}

// fallbackDNS answers what the fake DNS forwards (TXT, MX, PTR...) when the application asked
// the TUN's own DNS address, which exists only locally.
const fallbackDNS = "1.1.1.1:53"

// SetFakeDNS turns on DNS interception for whole-computer (TUN) mode: queries on port 53 are
// answered from m and connections to its fake IPs are sent to the exit by domain name.
func (s *SOCKS5Server) SetFakeDNS(m *fakedns.Mapper) {
	s.mu.Lock()
	s.fake = m
	s.mu.Unlock()
}

func (s *SOCKS5Server) fakeDNS() *fakedns.Mapper {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fake
}

// readSocksAddr reads ATYP, DST.ADDR and DST.PORT from a request.
func readSocksAddr(r io.Reader) (string, byte, error) {
	var atyp [1]byte
	if _, err := io.ReadFull(r, atyp[:]); err != nil {
		return "", 0, err
	}
	var host string
	switch atyp[0] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(r, ip); err != nil {
			return "", 0, err
		}
		host = net.IP(ip).String()
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", 0, err
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(r, name); err != nil {
			return "", 0, err
		}
		host = string(name)
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(r, ip); err != nil {
			return "", 0, err
		}
		host = net.IP(ip).String()
	default:
		return "", 0x08, ErrSocksAddressNotSupp
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", 0, err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), 0, nil
}

// parseUDPRequest splits a SOCKS5 UDP datagram: [RSV 2][FRAG][ATYP][ADDR][PORT][DATA].
// It returns the raw address bytes too, so replies can echo exactly what the client used.
func parseUDPRequest(b []byte) (addr string, rawAddr, data []byte, ok bool) {
	if len(b) < 4 || b[2] != 0 { // fragmentation is not supported (nobody uses it)
		return "", nil, nil, false
	}
	r := &sliceReader{b: b[3:]}
	addr, _, err := readSocksAddr(r)
	if err != nil {
		return "", nil, nil, false
	}
	end := len(b) - len(r.b)
	return addr, b[3:end], b[end:], true
}

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

func udpReply(rawAddr, data []byte) []byte {
	out := make([]byte, 0, 3+len(rawAddr)+len(data))
	out = append(out, 0, 0, 0)
	out = append(out, rawAddr...)
	return append(out, data...)
}

// handleUDPAssociate serves CMD 0x03. The association lives as long as the TCP control
// connection, as RFC 1928 requires.
func (s *SOCKS5Server) handleUDPAssociate(ctrl net.Conn) {
	ud, ok := s.currentDialer().(UDPDialer)
	if !ok {
		s.sendReply(ctrl, 0x07)
		return
	}
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		s.sendReply(ctrl, 0x01)
		return
	}
	defer pc.Close()

	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	sess, err := ud.DialExitUDP(ctx)
	cancel()
	if err != nil {
		s.noteRefusal("udp", err)
		s.sendReply(ctrl, 0x05)
		return
	}
	defer sess.Close()

	if err := s.sendReplyAddr(ctrl, 0x00, pc.LocalAddr().(*net.UDPAddr)); err != nil {
		return
	}
	_ = ctrl.SetDeadline(time.Time{})

	a := &udpAssoc{srv: s, pc: pc, sess: sess, fake: s.fakeDNS(), raw: make(map[string][]byte)}
	go a.fromExit()
	go a.fromClient()
	_, _ = io.Copy(io.Discard, ctrl) // returns when the client closes the control connection
}

type udpAssoc struct {
	srv  *SOCKS5Server
	pc   *net.UDPConn
	sess *UDPSession
	fake *fakedns.Mapper

	mu     sync.Mutex
	client *net.UDPAddr
	raw    map[string][]byte // address sent to the exit -> address bytes the client used
}

func (a *udpAssoc) clientAddr() *net.UDPAddr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *udpAssoc) fromClient() {
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := a.pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		a.mu.Lock()
		if a.client == nil {
			a.client = from // the first sender owns the association
		}
		own := a.client.IP.Equal(from.IP) && a.client.Port == from.Port
		a.mu.Unlock()
		if !own {
			continue
		}
		target, rawAddr, data, ok := parseUDPRequest(buf[:n])
		if !ok {
			continue
		}
		rawAddr = append([]byte(nil), rawAddr...)

		if a.fake != nil {
			_, port, _ := net.SplitHostPort(target)
			if port == "53" {
				if resp, handled := a.fake.Answer(data); handled {
					_, _ = a.pc.WriteToUDP(udpReply(rawAddr, resp), from)
					continue
				}
			}
			if host, _, _ := net.SplitHostPort(target); net.ParseIP(host) != nil && net.ParseIP(host).Equal(fakedns.DNSAddr) {
				target = fallbackDNS
			} else if t, ok := a.fake.TranslateHostPort(target); ok {
				target = t
			} else {
				continue // stale fake IP: nothing sensible to send it to
			}
		}

		a.mu.Lock()
		if len(a.raw) >= maxUDPTargets {
			a.raw = make(map[string][]byte)
		}
		a.raw[target] = rawAddr
		a.mu.Unlock()
		if err := a.sess.WriteTo(data, target); err != nil {
			a.pc.Close()
			return
		}
		a.srv.bytesTx.Add(uint64(len(data)))
	}
}

func (a *udpAssoc) fromExit() {
	for {
		data, from, err := a.sess.ReadFrom()
		if err != nil {
			a.pc.Close()
			return
		}
		a.mu.Lock()
		rawAddr, ok := a.raw[from]
		a.mu.Unlock()
		client := a.clientAddr()
		if !ok || client == nil {
			continue
		}
		if _, err := a.pc.WriteToUDP(udpReply(rawAddr, data), client); err != nil {
			return
		}
		a.srv.bytesRx.Add(uint64(len(data)))
	}
}
