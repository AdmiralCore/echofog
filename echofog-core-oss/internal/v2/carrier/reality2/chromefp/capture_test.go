package chromefp

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	quic "github.com/apernet/quic-go"
	utls "github.com/refraction-networking/utls"

	"github.com/echofog/echofog-core/internal/v2/carrier/quicx"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/profile"
)

// sampleHost is the SNI of every capture. Chrome resolves it to loopback by a host-resolver rule,
// so nothing leaves the machine.
const sampleHost = "chrome-sample.test"

// findChrome returns the stable Chrome binary and its version, or "" when there is none.
func findChrome() (path, version string) {
	var cands []string
	if p := os.Getenv("CHROME_BIN"); p != "" {
		cands = append(cands, p)
	}
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := os.Getenv(env); base != "" {
				cands = append(cands, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	case "darwin":
		cands = append(cands, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	default:
		for _, n := range []string{"google-chrome-stable", "google-chrome"} {
			if p, err := exec.LookPath(n); err == nil {
				cands = append(cands, p)
			}
		}
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		return p, chromeVersion(p)
	}
	return "", ""
}

// chromeVersion reads the version from the install directory (a folder named after it) on
// Windows, where chrome.exe --version would reach into a running browser session; elsewhere it
// asks the binary.
func chromeVersion(path string) string {
	ver := regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	if entries, err := os.ReadDir(filepath.Dir(path)); err == nil {
		for _, e := range entries {
			if e.IsDir() && ver.MatchString(e.Name()) {
				return e.Name()
			}
		}
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`).FindString(string(out)))
}

// runChrome starts headless Chrome on a fresh profile against the loopback ports and stops it when
// stop() returns. The resolver rule maps only sampleHost; every other name fails, so a stray
// background request cannot leave the machine.
func runChrome(t *testing.T, path string, port int, quicOnly bool, stop func() bool, limit time.Duration) {
	t.Helper()
	dir := t.TempDir()
	args := []string{
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir=" + dir,
		"--host-resolver-rules=MAP " + sampleHost + " 127.0.0.1, MAP * ~NOTFOUND",
		"--ignore-certificate-errors", "--disable-background-networking", "--disable-component-update",
		"--disable-sync", "--disable-default-apps", "--no-proxy-server", "--disable-extensions",
		"--disable-features=OptimizationHints,Translate,MediaRouter",
	}
	if quicOnly {
		args = append(args, "--enable-quic", fmt.Sprintf("--origin-to-force-quic-on=%s:%d", sampleHost, port))
	} else {
		args = append(args, "--disable-quic")
	}
	args = append(args, fmt.Sprintf("https://%s:%d/", sampleHost, port))
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-ctx.Done():
			break loop
		case <-tick.C:
			if stop() {
				break loop
			}
		}
	}
	killTree(cmd)
	<-done
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
		return
	}
	_ = cmd.Process.Kill()
}

// captureChromeTCP returns the ClientHello of one Chrome TLS connection to sampleHost.
func captureChromeTCP(t *testing.T, path string) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var hello []byte
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				if h := readTLSClientHello(c); h != nil {
					if s, err := Parse(h); err == nil && s.Transport == "tcp" && hasSNI(h) {
						mu.Lock()
						if hello == nil {
							hello = h
						}
						mu.Unlock()
					}
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	runChrome(t, path, port, false, func() bool { mu.Lock(); defer mu.Unlock(); return hello != nil }, 25*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if hello == nil {
		t.Fatal("Chrome sent no ClientHello to the loopback server")
	}
	return hello
}

func hasSNI(h []byte) bool { return bytes.Contains(h, []byte(sampleHost)) }

// readTLSClientHello reads one TLS record and returns the handshake message in it.
func readTLSClientHello(c net.Conn) []byte {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 22 {
		return nil
	}
	n := int(hdr[3])<<8 | int(hdr[4])
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil
	}
	return body
}

// captureChromeQUIC returns the ClientHello of Chrome's QUIC Initial packets.
func captureChromeQUIC(t *testing.T, path string) []byte {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var mu sync.Mutex
	var dgrams [][]byte
	var hello []byte
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			dgrams = append(dgrams, append([]byte(nil), buf[:n]...))
			if h, err := profile.ClientHelloFromInitials(dgrams); err == nil && hello == nil && hasSNI(h) {
				hello = h
			}
			mu.Unlock()
		}
	}()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	runChrome(t, path, port, true, func() bool { mu.Lock(); defer mu.Unlock(); return hello != nil }, 25*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if hello == nil {
		t.Fatal("Chrome sent no QUIC Initial with a ClientHello to the loopback server")
	}
	return hello
}

// ourTCP returns the ClientHello of the profile the reality2-tcp dialer uses, built the way the
// dialer builds it (token in the session id).
func ourTCP(t *testing.T) []byte {
	t.Helper()
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	uc := utls.UClient(cli, &utls.Config{ServerName: sampleHost, InsecureSkipVerify: true}, utls.HelloChrome_Auto)
	mask, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reality2.PrepareTCPHello(uc, mask.PublicKey().Bytes(), "127.0.0.1:443", nil); err != nil {
		t.Fatal(err)
	}
	go func() { _ = uc.Handshake() }()
	_ = srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	h := readTLSClientHello(srv)
	if h == nil {
		t.Fatal("no ClientHello from our TCP profile")
	}
	return h
}

// ourQUIC returns the ClientHello of the reality2-quic client config (ChromeParrot).
func ourQUIC(t *testing.T) []byte {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var mu sync.Mutex
	var dgrams [][]byte
	go func() {
		buf := make([]byte, 2048)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			dgrams = append(dgrams, append([]byte(nil), buf[:n]...))
			mu.Unlock()
		}
	}()
	cliPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cliPC.Close()
	clientTLS, _ := quicx.Reality2TLSConfig(0, nil)
	clientTLS.ServerName = sampleHost
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, _ = quic.Dial(ctx, cliPC, pc.LocalAddr(), clientTLS, quicx.Reality2QUICConfig()) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		h, err := profile.ClientHelloFromInitials(dgrams)
		mu.Unlock()
		if err == nil {
			return h
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no QUIC Initial from our profile")
	return nil
}
