package scenarios

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/testbed/browserprofiles"
	"github.com/echofog/echofog-core/testbed/censor"
)

// The browser control: Chrome-shaped TLS (uTLS HelloChrome_Auto, as testbed/v2carrier does) fetching
// pages of varied size from a local TLS server, through the same TCP tap as the carriers. It is the
// negative sample for the false-positive rate and the reference that D6 and D7 calibrate on.

const (
	browserUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	pagePath    = "/p?n="
	maxPageSize = 200 << 10
)

func browserRequest(host string, n int) string {
	return fmt.Sprintf("GET %s%d HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\n"+
		"Accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8\r\n"+
		"Accept-Language: en-US,en;q=0.9\r\nAccept-Encoding: gzip, deflate, br\r\nCookie: session=%s\r\n\r\n",
		pagePath, n, host, browserUA, strings.Repeat("a1b2c3d4", 5+n%40))
}

func fetchPage(c net.Conn, br *bufio.Reader, host string, n int) error {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, browserRequest(host, n)); err != nil {
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

// servePages answers GET /p?n=N with N bytes, as many times as the client asks.
func servePages(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(c)
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
		n = max(0, min(n, maxPageSize))
		body := make([]byte, n)
		for i := range body {
			body[i] = byte('a' + i%26)
		}
		hdr := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\n\r\n", n)
		if _, err := c.Write(append([]byte(hdr), body...)); err != nil {
			return
		}
	}
}

// captureBrowser makes visits connections as a Chrome-shaped client, each with a few page loads
// and a short think time, and returns the flows the censor read.
func captureBrowser(ctx context.Context, visits int, seed int64) ([]censor.Flow, error) {
	cert, err := selfSignedCert(webName)
	if err != nil {
		return nil, err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go servePages(c)
		}
	}()
	pw, wait := newCapture()
	tap, err := NewTCPTap(ln.Addr().String(), fakeWeb, pw)
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	var runErr error
	for i := 0; i < visits && runErr == nil && ctx.Err() == nil; i++ {
		runErr = visit(tap.Addr(), rng)
	}
	_ = tap.Close()
	flows, err := wait()
	if runErr != nil {
		return nil, runErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return flows, err
}

func visit(addr string, rng *rand.Rand) error {
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	u := utls.UClient(raw, &utls.Config{ServerName: webName, InsecureSkipVerify: true}, utls.HelloChrome_Auto) // #nosec G402 -- the lab server is self-signed
	_ = u.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		_ = raw.Close()
		return err
	}
	defer u.Close()
	br := bufio.NewReader(u)
	for k, n := 0, 2+rng.Intn(5); k < n; k++ {
		if err := fetchPage(u, br, webName, 300+rng.Intn(60000)); err != nil {
			return err
		}
		time.Sleep(time.Duration(5+rng.Intn(35)) * time.Millisecond)
	}
	return nil
}

// censorConfig is the censor's offline knowledge for the run: what each fake address hosts, DNS for
// those names, a block list, and the browser fingerprints of testbed/browserprofiles (the file the
// launchpad uses; when it cannot be read the second result says so and D4 compares with nothing).
func censorConfig() (censor.DetectorConfig, string) {
	hosted := map[netip.Addr][]string{
		fakeQUIC.Addr(): {coverName}, fakeTCP.Addr(): {coverName}, fakeWS2.Addr(): {cdnName}, fakeWeb.Addr(): {webName},
	}
	asn := map[netip.Addr]string{}
	for ip := range hosted {
		asn[ip] = "AS64500"
	}
	cfg := censor.DetectorConfig{
		BlockedSNI: map[string]bool{"blocked.example": true},
		Hosted:     hosted,
		ServerASN:  asn,
		DNS: map[string]censor.ResolvedName{
			coverName: {IPs: []netip.Addr{fakeQUIC.Addr(), fakeTCP.Addr()}, ASN: "AS64500"},
			cdnName:   {IPs: []netip.Addr{fakeWS2.Addr()}, ASN: "AS64500"},
			webName:   {IPs: []netip.Addr{fakeWeb.Addr()}, ASN: "AS64500"},
		},
		Seed: 7,
	}
	_, here, _, _ := runtime.Caller(0)
	db, err := browserprofiles.Load(filepath.Join(filepath.Dir(here), "..", "browserprofiles", "profiles.json"))
	if err != nil {
		return cfg, "the browser profile file could not be read (" + err.Error() + "): D4 had no browser fingerprints to compare with"
	}
	cfg.BrowserJA4, cfg.BrowserJA3 = db.BrowserJA4(), db.BrowserJA3()
	return cfg, ""
}
