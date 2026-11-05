package browserprofiles

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/apernet/quic-go"
	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/testbed/censor"
)

const captureSNI = "cover.lab"

func profileFromTLS(source, sourceVersion string, fp censor.Fingerprint) Profile {
	return Profile{
		Source: source, SourceVersion: sourceVersion,
		TLS: &TLSProfile{
			JA3: fp.JA3, JA4: fp.JA4, Grease: fp.Grease, Ciphers: fp.Ciphers,
			Extensions: fp.Extensions, ALPN: strings.Join(fp.ALPN, ","), Version: fp.Version,
			PQ: fp.PQ, ECH: fp.ECH, Bytes: fp.Bytes, SHA256: fp.SHA256,
		},
	}
}

// captureTLSChrome uses the uTLS Chrome preset. This is not a browser binary; the entry says so.
func captureTLSChrome() (Profile, error) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	go func() {
		defer c1.Close()
		u := utls.UClient(c1, &utls.Config{ // #nosec G402 -- a local capture, no secrets
			ServerName: captureSNI, InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"},
		}, utls.HelloChrome_Auto)
		_ = u.Handshake()
	}()
	fp, err := readTLSHello(c2)
	if err != nil {
		return Profile{}, err
	}
	return profileFromTLS(SourceUTLSChrome, moduleVersion("github.com/refraction-networking/utls"), fp), nil
}

// captureCryptoTLS is the Go standard library: no GREASE, the honest "library, not a browser" case.
func captureCryptoTLS() (Profile, error) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	go func() {
		defer c1.Close()
		_ = tls.Client(c1, &tls.Config{ // #nosec G402 -- a local capture
			ServerName: captureSNI, InsecureSkipVerify: true, NextProtos: []string{"h2"},
		}).Handshake()
	}()
	fp, err := readTLSHello(c2)
	if err != nil {
		return Profile{}, err
	}
	return profileFromTLS(SourceCryptoTLS, moduleVersion(""), fp), nil
}

// captureQUICChrome takes the Initial quic-go sends with ChromeParrot, the browser-like QUIC
// control. It does not finish a handshake; the first datagrams are all a censor sees anyway.
func captureQUICChrome(ctx context.Context) (Profile, error) {
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return Profile{}, err
	}
	defer sink.Close()
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	go func() {
		_, _ = quic.DialAddr(dctx, sink.LocalAddr().String(), &tls.Config{ // #nosec G402 -- a local sink
			ServerName: captureSNI, NextProtos: []string{"h3"}, InsecureSkipVerify: true,
		}, &quic.Config{ChromeParrot: true})
	}()
	var datagrams [][]byte
	buf := make([]byte, 4096)
	for i := 0; i < 4; i++ {
		_ = sink.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := sink.ReadFrom(buf)
		if err != nil {
			break
		}
		datagrams = append(datagrams, append([]byte(nil), buf[:n]...))
		if fp, ok := censor.ParseQUICClientHello(datagrams...); ok {
			q := censor.ParseQUICDatagram(datagrams[0])
			return Profile{
				Source: SourceQUICChrome, SourceVersion: moduleVersion("github.com/apernet/quic-go"),
				QUIC: &QUICProfile{
					Version: q.Version, FirstSize: q.FirstSize, FixedBit: q.FixedBit, Decrypted: q.Decrypted,
					JA4: fp.JA4, SNI: fp.SNI, ALPN: fp.ALPN,
				},
			}, nil
		}
	}
	return Profile{}, errors.New("no QUIC ClientHello captured")
}

// captureCurl records a real curl ClientHello. curl is not a browser; it is the honest small tool.
func captureCurl(ctx context.Context) (Profile, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Profile{}, err
	}
	defer ln.Close()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer conn.Close()
		data, err := readTLSHelloRaw(conn)
		ch <- result{data: data, err: err}
	}()
	cmd := exec.CommandContext(ctx, "curl", "-sk", "--tlsv1.3", "--max-time", "3", "https://"+ln.Addr().String()+"/")
	_ = cmd.Run()
	select {
	case r := <-ch:
		if r.err != nil || len(r.data) == 0 {
			return Profile{}, fmt.Errorf("curl hello: %v", r.err)
		}
		fp, ok := censor.ParseTLSClientHello(r.data)
		if !ok {
			return Profile{}, errors.New("curl hello did not parse")
		}
		return profileFromTLS(SourceCurl, curlVersion(), fp), nil
	case <-time.After(4 * time.Second):
		return Profile{}, errors.New("curl hello: timeout")
	case <-ctx.Done():
		return Profile{}, ctx.Err()
	}
}

// captureChrome uses a real Chrome, when one is installed. It is a headless visit to a local
// listener: the ClientHello is sent before the certificate matters, so no certificate is served.
func captureChrome(ctx context.Context) (Profile, error) {
	exe := chromePath()
	if exe == "" {
		return Profile{}, errors.New("chrome: not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Profile{}, err
	}
	defer ln.Close()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer conn.Close()
		data, err := readTLSHelloRaw(conn)
		ch <- result{data: data, err: err}
	}()
	dir, err := os.MkdirTemp("", "echofog-chrome-")
	if err != nil {
		return Profile{}, err
	}
	defer os.RemoveAll(dir)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	url := "https://" + captureSNI + ":" + port + "/"
	cmd := exec.CommandContext(cctx, exe,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--ignore-certificate-errors", "--no-proxy-server",
		"--host-resolver-rules=MAP "+captureSNI+" 127.0.0.1",
		"--user-data-dir="+dir, url)
	_ = cmd.Start()
	defer func() { _ = cmd.Process.Kill() }()
	select {
	case r := <-ch:
		if r.err != nil || len(r.data) == 0 {
			return Profile{}, fmt.Errorf("chrome hello: %v", r.err)
		}
		fp, ok := censor.ParseTLSClientHello(r.data)
		if !ok {
			return Profile{}, errors.New("chrome hello did not parse")
		}
		return profileFromTLS(SourceChrome, chromeVersion(exe), fp), nil
	case <-cctx.Done():
		return Profile{}, errors.New("chrome hello: timeout")
	}
}

func readTLSHello(conn net.Conn) (censor.Fingerprint, error) {
	data, err := readTLSHelloRaw(conn)
	if err != nil {
		return censor.Fingerprint{}, err
	}
	fp, ok := censor.ParseTLSClientHello(data)
	if !ok {
		return censor.Fingerprint{}, errors.New("the stream does not start with a ClientHello")
	}
	return fp, nil
}

func readTLSHelloRaw(conn net.Conn) ([]byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8192)
	var data []byte
	for len(data) < 64<<10 {
		n, err := conn.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			if _, ok := censor.ParseTLSClientHello(data); ok {
				return data, nil
			}
		}
		if err != nil {
			if len(data) > 0 {
				return data, nil
			}
			return nil, err
		}
	}
	return data, nil
}

func chromePath() string {
	if p, err := exec.LookPath("chrome"); err == nil {
		return p
	}
	paths := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func chromeVersion(exe string) string {
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		return filepath.Base(exe) + " (version not reported)"
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" || strings.Contains(strings.ToLower(line), "existing browser") {
		return filepath.Base(exe) + " (version not reported: an instance was already running)"
	}
	return line
}

func curlVersion() string {
	out, err := exec.Command("curl", "--version").Output()
	if err != nil {
		return "curl"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if path == "" {
		return info.GoVersion
	}
	for _, m := range info.Deps {
		if m.Path == path {
			return m.Version
		}
	}
	return ""
}
