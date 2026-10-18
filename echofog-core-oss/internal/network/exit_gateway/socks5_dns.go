package exit_gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"github.com/echofog/echofog-core/internal/network/fakedns"
)

// replyFor turns a failed exit dial into the SOCKS5 reply code that says what happened, so
// curl, tun2socks and browsers report the right thing; noteRefusal keeps the detailed reason.
func replyFor(err error) byte {
	switch {
	case errors.Is(err, ErrExitDenied), errors.Is(err, ErrPaymentRequired), errors.Is(err, ErrExitDisabled):
		return 0x02 // connection not allowed by ruleset
	case errors.Is(err, ErrExitDialFailed):
		return 0x04 // host unreachable
	case errors.Is(err, context.DeadlineExceeded):
		return 0x06 // TTL expired
	default:
		return 0x01 // general failure: the exit itself is out of reach
	}
}

// isFakeResolver: the target is the TUN's own DNS address, which exists only on this machine.
func isFakeResolver(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || port != "53" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.Equal(fakedns.DNSAddr)
}

// maxDNSMessage bounds one DNS-over-TCP message (RFC 1035: a 16-bit length).
const maxDNSMessage = 65535

// serveTCPDNS answers DNS over TCP sent to the TUN's resolver, the way the UDP path does: A and
// AAAA from the fake mapper, the rest forwarded to fallbackDNS through the exit. Resolvers fall
// back to TCP for truncated answers, so refusing it breaks large replies.
func (s *SOCKS5Server) serveTCPDNS(c net.Conn, fake *fakedns.Mapper) {
	if err := s.sendReply(c, 0x00); err != nil {
		return
	}
	var upstream io.ReadWriteCloser
	// bounds the exit's answer to the upstream dial, which comes with the first reply (fast open)
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	defer func() {
		if upstream != nil {
			upstream.Close()
		}
	}()
	for {
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		msg, err := readDNSTCP(c)
		if err != nil {
			return
		}
		resp, handled := fake.Answer(msg)
		if !handled {
			if upstream == nil {
				d := s.currentDialer()
				if d == nil {
					return
				}
				upstream, err = d.DialExitStream(ctx, fallbackDNS)
				if err != nil {
					upstream = nil
					s.noteRefusal(fallbackDNS, err)
					return
				}
				if v, ok := upstream.(VerdictNotifier); ok {
					v.OnVerdict(func(err error) {
						if err != nil {
							s.noteRefusal(fallbackDNS, err)
						}
					})
				}
			}
			if err := writeDNSTCP(upstream, msg); err != nil {
				return
			}
			if resp, err = readDNSTCP(upstream); err != nil {
				return
			}
		}
		if err := writeDNSTCP(c, resp); err != nil {
			return
		}
	}
}

func readDNSTCP(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(l[:]))
	if n == 0 || n > maxDNSMessage {
		return nil, errors.New("bad DNS message length")
	}
	msg := make([]byte, n)
	_, err := io.ReadFull(r, msg)
	return msg, err
}

func writeDNSTCP(w io.Writer, msg []byte) error {
	if len(msg) > maxDNSMessage {
		return errors.New("DNS message too long")
	}
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	_, err := w.Write(out)
	return err
}
