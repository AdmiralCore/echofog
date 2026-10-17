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
//
// It answers at once and reaches the exit only when the first datagram needs it. In
// whole-computer mode every DNS query arrives as its own association, and the fake DNS answers
// most of them here: they must not wait for the exit, cost it a stream, or fail with it.
func (s *SOCKS5Server) handleUDPAssociate(ctrl net.Conn) {
	ud, ok := s.currentDialer().(UDPDialer)
	if !ok {
		s.sendReply(ctrl, 0x07)
		return
	}
	select {
	case s.udpSlots <- struct{}{}:
		defer func() { <-s.udpSlots }()
	default:
		s.sendReply(ctrl, 0x01) // a local app opened far too many associations
		return
	}
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		s.sendReply(ctrl, 0x01)
		return
	}
	if err := s.sendReplyAddr(ctrl, 0x00, pc.LocalAddr().(*net.UDPAddr)); err != nil {
		pc.Close()
		return
	}
	_ = ctrl.SetDeadline(time.Time{})

	a := &udpAssoc{srv: s, ctrl: ctrl, pc: pc, dialer: ud, fake: s.fakeDNS(), raw: make(map[string][]byte)}
	defer a.close()
	go a.fromClient()
	_, _ = io.Copy(io.Discard, ctrl) // returns when the client closes the control connection
}

// pendingUDP is how many datagrams wait for the exit session while it is being opened.
const pendingUDP = 32

type udpAssoc struct {
	srv    *SOCKS5Server
	ctrl   net.Conn
	pc     *net.UDPConn
	dialer UDPDialer
	fake   *fakedns.Mapper

	mu      sync.Mutex
	client  *net.UDPAddr
	raw     map[string][]byte // address sent to the exit -> address bytes the client used
	sess    *UDPSession
	dialing bool
	pending []pendingDatagram
	closed  bool
}

type pendingDatagram struct {
	data   []byte
	target string
}

func (a *udpAssoc) clientAddr() *net.UDPAddr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// close ends the association: the local port, the exit session and the control connection,
// so the client learns that the association is gone instead of sending into a dead port.
func (a *udpAssoc) close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	sess := a.sess
	a.mu.Unlock()
	_ = a.pc.Close()
	_ = a.ctrl.Close()
	if sess != nil {
		_ = sess.Close()
	}
}

// send hands a datagram to the exit, opening the session on first use.
func (a *udpAssoc) send(data []byte, target string) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	if sess := a.sess; sess != nil {
		a.mu.Unlock()
		if err := sess.WriteTo(data, target); err != nil {
			a.close()
			return
		}
		a.srv.bytesTx.Add(uint64(len(data)))
		return
	}
	if len(a.pending) < pendingUDP {
		a.pending = append(a.pending, pendingDatagram{append([]byte(nil), data...), target})
	}
	if a.dialing {
		a.mu.Unlock()
		return
	}
	a.dialing = true
	a.mu.Unlock()
	go a.open()
}

func (a *udpAssoc) open() {
	ctx, cancel := context.WithTimeout(a.srv.ctx, 10*time.Second)
	sess, err := a.dialer.DialExitUDP(ctx)
	cancel()
	if err != nil {
		a.srv.noteRefusal("udp", err)
		a.close()
		return
	}
	a.srv.health.ok()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = sess.Close()
		return
	}
	a.sess = sess
	queued := a.pending
	a.pending = nil
	a.mu.Unlock()
	go a.fromExit(sess)
	for _, d := range queued {
		if err := sess.WriteTo(d.data, d.target); err != nil {
			a.close()
			return
		}
		a.srv.bytesTx.Add(uint64(len(d.data)))
	}
}

func (a *udpAssoc) fromClient() {
	defer a.close()
	buf := make([]byte, maxDatagram)
	for {
		n, from, err := a.pc.ReadFromUDP(buf)
		if err != nil {
			if udpReadTransient(err) {
				continue
			}
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
		a.send(data, target)
	}
}

func (a *udpAssoc) fromExit(sess *UDPSession) {
	defer a.close()
	for {
		data, from, err := sess.ReadFrom()
		if err != nil {
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
			if udpReadTransient(err) {
				continue
			}
			return
		}
		a.srv.bytesRx.Add(uint64(len(data)))
	}
}
