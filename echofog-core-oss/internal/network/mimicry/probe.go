package mimicry

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"
)

// A node mimics a real site (the SNI it presents). What a passive observer can fingerprint of our
// server's answer (JA3S) comes from the ServerHello: the TLS version, the chosen cipher, and the
// chosen key_share group. These must match what the real site would answer to the same kind of
// ClientHello, or the camouflage is inconsistent. ServerProfile captures those fields, measured
// both from the real site and from our own server, so a node can check and report at start-up that
// they line up. Byte-exact ServerHello forgery (full REALITY) would need a patched TLS stack; this
// aligns and verifies the parts a passive fingerprint actually reads.
//
// The probe uses a client that offers the same groups and ciphers a modern Chrome does (the hybrid
// post-quantum X25519MLKEM768 first), so a site chooses the same group and cipher it would for
// Chrome. The exact ClientHello bytes do not matter here: only which group/cipher/version the
// server picks, which depends on what is offered, not on the byte-for-byte hello.
var chromeGroups = []tls.CurveID{tls.X25519MLKEM768, tls.X25519, tls.CurveP256}

// ServerProfile is what a passive observer sees of a TLS server's ServerHello.
type ServerProfile struct {
	Version uint16
	Cipher  uint16
	Group   tls.CurveID
	ALPN    string
}

func (p ServerProfile) String() string {
	return fmt.Sprintf("TLS %s, %s, group %d, alpn %q", tlsVersionName(p.Version), tls.CipherSuiteName(p.Cipher), p.Group, p.ALPN)
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func chromeLikeClientConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		CurvePreferences:   chromeGroups,
		NextProtos:         []string{"h2", "http/1.1"},
	}
}

func profileFrom(cs tls.ConnectionState) ServerProfile {
	return ServerProfile{Version: cs.Version, Cipher: cs.CipherSuite, Group: cs.CurveID, ALPN: cs.NegotiatedProtocol}
}

// ProbeSite connects to a real site and reports what its ServerHello looks like.
func ProbeSite(ctx context.Context, addr, serverName string) (ServerProfile, error) {
	d := tls.Dialer{Config: chromeLikeClientConfig(serverName)}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return ServerProfile{}, err
	}
	defer conn.Close()
	return profileFrom(conn.(*tls.Conn).ConnectionState()), nil
}

// OwnServerProfile measures what our own server, built with tlsCfg, answers to a Chrome-like
// ClientHello, by handshaking against it over an in-memory pipe. No network is used.
func OwnServerProfile(tlsCfg *tls.Config) (ServerProfile, error) {
	cliRaw, srvRaw := net.Pipe()
	defer cliRaw.Close()
	defer srvRaw.Close()
	type res struct {
		cs  tls.ConnectionState
		err error
	}
	done := make(chan res, 1)
	go func() {
		s := tls.Server(srvRaw, tlsCfg)
		_ = s.SetDeadline(time.Now().Add(5 * time.Second))
		if err := s.Handshake(); err != nil {
			done <- res{err: err}
			return
		}
		done <- res{cs: s.ConnectionState()}
	}()
	c := tls.Client(cliRaw, chromeLikeClientConfig("www.microsoft.com"))
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.Handshake(); err != nil {
		return ServerProfile{}, err
	}
	r := <-done
	if r.err != nil {
		return ServerProfile{}, r.err
	}
	return profileFrom(r.cs), nil
}

// CheckServerHelloConformance compares our server's ServerHello to the real site's and returns a
// one-line verdict and whether the passive-visible fields (version, cipher, group) all match.
func CheckServerHelloConformance(ctx context.Context, tlsCfg *tls.Config, dest, serverName string) (string, bool) {
	site, err := ProbeSite(ctx, dest, serverName)
	if err != nil {
		return fmt.Sprintf("could not reach the mimicked site %s to check the ServerHello: %v", dest, err), false
	}
	ours, err := OwnServerProfile(tlsCfg)
	if err != nil {
		return fmt.Sprintf("could not measure our own ServerHello: %v", err), false
	}
	if ours.Version == site.Version && ours.Cipher == site.Cipher && ours.Group == site.Group {
		return fmt.Sprintf("ServerHello matches %s (%s)", serverName, ours), true
	}
	return fmt.Sprintf("ServerHello differs from %s: ours=[%s] site=[%s]; choose an SNI whose server answers like ours, or the passive fingerprint will not match", serverName, ours, site), false
}
