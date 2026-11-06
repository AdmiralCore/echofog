package probe

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// visited is what a stranger who opens the site learns: the certificate, the protocol choices and
// the page.
type visited struct {
	cert, alpn, page Answer
}

// visit makes an ordinary HTTPS visit to addr as a client that names sni, and reduces it.
func visit(ctx context.Context, d Dialer, addr, sni string) visited {
	var v visited
	fail := func(err error) visited {
		a := Answer{Class: "visit-failed", Detail: errClass(err)}
		return visited{cert: a, alpn: a, page: a}
	}
	var state tls.ConnectionState
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			raw, err := dialWith(d, ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			c := tls.Client(raw, &tls.Config{ // #nosec G402 -- the lab covers are self-signed
				ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"},
			})
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			if err := c.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			_ = c.SetDeadline(time.Time{})
			state = c.ConnectionState()
			return c, nil
		},
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+sni+"/", nil)
	if err != nil {
		return fail(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if len(state.PeerCertificates) > 0 {
		sum := sha256.Sum256(state.PeerCertificates[0].Raw)
		v.cert = Answer{Class: "cert", Detail: hex.EncodeToString(sum[:8])}
	} else {
		v.cert = Answer{Class: "cert", Detail: "none"}
	}
	v.alpn = Answer{Class: "alpn", Detail: fmt.Sprintf("%s/%04x", state.NegotiatedProtocol, state.Version)}
	bs := sha256.Sum256(body)
	v.page = Answer{Class: "http", Detail: fmt.Sprintf("%d server=%s type=%s body=%s",
		resp.StatusCode, resp.Header.Get("Server"), resp.Header.Get("Content-Type"), hex.EncodeToString(bs[:6]))}
	return v
}

func errClass(err error) string {
	if err == nil {
		return ""
	}
	a := Reduce("tcp", nil, err)
	if a.Class == "closed" || a.Class == "silence" || a.Class == "reset" || a.Class == "refused" {
		return a.Class
	}
	return "error"
}
