package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The control traffic of the launchpad: a browser-like visit for the false-positive rate of D5–D8,
// and the tunnels those detectors exist for. All of it is ordinary TLS made by real libraries
// against the lab server, so a detector is tested on real record sizes and timings, not on a
// generator's idea of them.

// recordConn lets a test see the client side of every connection a lab mode opens; the harness
// leaves it alone.
var recordConn = func(c net.Conn) net.Conn { return c }

const (
	httpPath    = "/p?n="
	maxBodySize = 200 << 10
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// cookie is a session cookie whose length follows n, so the requests of one visit differ in size.
func cookie(n int) string {
	return "session=" + strings.Repeat("a1b2c3d4", 5+n%40)
}

// request is one HTTP/1.1 GET for n bytes of answer, with the headers of a real page load: a long
// user agent, the Accept family and a cookie, which is what makes a request several hundred bytes
// and not a hundred.
func request(host string, n int) string {
	return fmt.Sprintf("GET %s%d HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\n"+
		"Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8\r\n"+
		"Accept-Language: en-US,en;q=0.9\r\nAccept-Encoding: gzip, deflate, br\r\nCookie: %s\r\n\r\n",
		httpPath, n, host, browserUA, cookie(n))
}

// fetch sends the request on c and reads the whole answer.
func fetch(c net.Conn, br *bufio.Reader, host string, n int) error {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, request(host, n)); err != nil {
		return err
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	got, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return err
	}
	if int(got) != n {
		return fmt.Errorf("asked for %d bytes, got %d", n, got)
	}
	return nil
}

func chromeConn(raw net.Conn, sni string) (*utls.UConn, error) {
	u := utls.UClient(recordConn(raw), &utls.Config{ // #nosec G402 -- the lab server is self-signed
		ServerName: sni, InsecureSkipVerify: true,
	}, utls.HelloChrome_Auto)
	_ = u.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return u, nil
}

// labVisit makes visits ordinary HTTPS visits as a Chrome-shaped client: a connection, a few
// requests with a short think time between them, the answers of varying size.
func labVisit(peer, sni string, visits int, seed int64) (result, error) {
	res := result{Mode: "visit", Carrier: "utls-chrome-visit"}
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < visits; i++ {
		raw, err := net.DialTimeout("tcp", peer, 3*time.Second)
		if err != nil {
			return res, err
		}
		u, err := chromeConn(raw, sni)
		if err != nil {
			return res, err
		}
		br := bufio.NewReader(u)
		for k, n := 0, 3+rng.Intn(6); k < n; k++ {
			if err := fetch(u, br, sni, 300+rng.Intn(60000)); err != nil {
				_ = u.Close()
				return res, err
			}
			res.Packets++
			time.Sleep(time.Duration(5+rng.Intn(35)) * time.Millisecond)
		}
		_ = u.Close()
		res.Connections++
		// a person opens a page every few seconds; 25 connections in a burst would be a storm to D8
		// in the control itself (found on the first live run)
		time.Sleep(time.Duration(700+rng.Intn(500)) * time.Millisecond)
	}
	return res, nil
}

// tlsInTLS opens a TLS connection to the server and runs a second TLS handshake and a few
// requests inside it: the traffic D5 exists for.
func tlsInTLS(peer, sni string, seed int64) (result, error) {
	res := result{Mode: "tls-in-tls", Carrier: "tls-in-tls-control", Synthetic: true}
	rng := rand.New(rand.NewSource(seed))
	raw, err := net.DialTimeout("tcp", peer, 3*time.Second)
	if err != nil {
		return res, err
	}
	outer, err := chromeConn(raw, sni)
	if err != nil {
		return res, err
	}
	defer outer.Close()
	inner := tls.Client(outer, &tls.Config{ServerName: sni, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) // #nosec G402 -- the lab server is self-signed
	_ = inner.SetDeadline(time.Now().Add(5 * time.Second))
	if err := inner.Handshake(); err != nil {
		return res, fmt.Errorf("inner handshake: %w", err)
	}
	br := bufio.NewReader(inner)
	for k, n := 0, 2+rng.Intn(4); k < n; k++ {
		if err := fetch(inner, br, sni, 300+rng.Intn(40000)); err != nil {
			return res, err
		}
		res.Packets++
		time.Sleep(time.Duration(5+rng.Intn(35)) * time.Millisecond)
	}
	res.Connections = 1
	return res, nil
}

// constSize sends 1400-byte writes at a fixed pace over a TLS connection and reads the echo.
func constSize(peer, sni string, seed int64) (result, error) {
	res := result{Mode: "const-size", Carrier: "const-size-control", Synthetic: true}
	raw, err := net.DialTimeout("tcp", peer, 3*time.Second)
	if err != nil {
		return res, err
	}
	c, err := chromeConn(raw, sni)
	if err != nil {
		return res, err
	}
	defer c.Close()
	go func() { _, _ = io.Copy(io.Discard, c) }()
	chunk := make([]byte, 1400)
	rand.New(rand.NewSource(seed)).Read(chunk)
	chunk[0] = 'C' // tells the lab server to echo
	for i := 0; i < 60; i++ {
		if _, err := c.Write(chunk); err != nil {
			return res, err
		}
		res.Bytes += int64(len(chunk))
		res.Packets++
		time.Sleep(20 * time.Millisecond)
	}
	res.Connections = 1
	return res, nil
}

// connStorm opens n short TLS connections to the server in a few seconds.
func connStorm(peer, sni string, n int) (result, error) {
	res := result{Mode: "conn-storm", Carrier: "conn-storm-control", Synthetic: true}
	for i := 0; i < n; i++ {
		raw, err := net.DialTimeout("tcp", peer, 3*time.Second)
		if err != nil {
			return res, err
		}
		c := tls.Client(recordConn(raw), &tls.Config{ServerName: sni, InsecureSkipVerify: true}) // #nosec G402 -- the lab server is self-signed
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if err := c.Handshake(); err != nil {
			_ = raw.Close()
			return res, err
		}
		_, _ = c.Write([]byte("hello"))
		_ = c.Close()
		res.Connections++
		time.Sleep(100 * time.Millisecond)
	}
	return res, nil
}

// ---------------------------------------------------------------------------- server side

// serveConn answers one accepted TLS connection of the lab server by what its first byte says: a
// second TLS handshake (the tunnel control), an HTTP request (the browser control), 'C' (echo, the
// fixed-size control) or anything else (the old behaviour: read, say "ok").
func serveConn(conn net.Conn, cert tls.Certificate) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	switch {
	case first[0] == 0x16:
		inner := tls.Server(&bufConn{Conn: conn, r: br}, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		if err := inner.Handshake(); err != nil {
			return
		}
		serveHTTP(inner, bufio.NewReader(inner))
	case first[0] == 'G':
		serveHTTP(conn, br)
	case first[0] == 'C':
		_, _ = io.Copy(conn, br)
	default:
		buf := make([]byte, 4096)
		_, _ = io.ReadFull(br, buf)
		_, _ = conn.Write([]byte("ok"))
	}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// serveHTTP answers GET /p?n=N with N bytes, as many times as the client asks.
func serveHTTP(w io.Writer, br *bufio.Reader) {
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(h) == "" {
				break
			}
		}
		n := 0
		if f := strings.Fields(line); len(f) >= 2 {
			if i := strings.Index(f[1], "n="); i >= 0 {
				n, _ = strconv.Atoi(f[1][i+2:])
			}
		}
		n = max(0, min(n, maxBodySize))
		body := make([]byte, n)
		for i := range body {
			body[i] = byte('a' + i%26)
		}
		out := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\n\r\n", n)
		if _, err := w.Write(append([]byte(out), body...)); err != nil {
			return
		}
	}
}
