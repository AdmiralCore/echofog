//go:build windows || linux

package tunmode

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

func Supported() bool { return true }

const mtu = 1500

// Addresses of the TUN interface; DNS points into the fake pool the node answers from.
const (
	tunCIDR4 = "198.18.0.1/15"
	tunAddr4 = "198.18.0.1"
	tunMask4 = "255.254.0.0"
	tunDNS4  = "198.18.0.2"
	tunCIDR6 = "fdfe:dcba:9876::1/126"
)

// HelperMain runs the elevated helper; args follow HelperFlag. It returns the exit code.
func HelperMain(args []string) int {
	hardenHelper()
	fs := flag.NewFlagSet("tun-helper", flag.ContinueOnError)
	socks := fs.String("socks", "", "node SOCKS5 address")
	ctl := fs.String("ctl", "", "node control address")
	token := fs.String("token", "", "control token")
	bypass := fs.Int("bypass-udp-port", 0, "node UDP port kept off the TUN")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	for _, a := range []string{*socks, *ctl} {
		if host, _, err := net.SplitHostPort(a); err != nil || host != "127.0.0.1" {
			fmt.Fprintln(os.Stderr, "tun-helper: addresses must be on 127.0.0.1")
			return 2
		}
	}
	conn, err := net.DialTimeout("tcp", *ctl, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tun-helper:", err)
		return 1
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, *token+"\n"); err != nil {
		return 1
	}
	fail := func(err error) int {
		_, _ = io.WriteString(conn, linePrefErr+err.Error()+"\n")
		return 1
	}

	if err := preflight(); err != nil {
		return fail(err)
	}
	engine.Insert(&engine.Key{
		Device:     deviceURL(),
		Proxy:      "socks5://" + *socks,
		MTU:        mtu,
		LogLevel:   "warn",
		UDPTimeout: time.Minute,
	})
	engine.Start() // exits the process on failure; the node then sees the control connection drop

	teardown, err := configure(*bypass)
	// order matters on the way out: device first, then routing leftovers, then the DNS cache,
	// so nothing re-resolves into a fake IP that no longer leads anywhere
	defer func() {
		engine.Stop()
		if teardown != nil {
			teardown()
		}
		flushDNS()
	}()
	if err != nil {
		return fail(fmt.Errorf("configure the TUN device: %w", err))
	}
	flushDNS() // cached real addresses would bypass the fake DNS and lose the domain name
	if _, err := io.WriteString(conn, lineUp+"\n"); err != nil {
		return 1
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	closed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, conn); close(closed) }()
	select {
	case <-closed:
	case <-sig:
	}
	return 0
}

func waitInterface(name string) (*net.Interface, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		ifc, err := net.InterfaceByName(name)
		if err == nil {
			return ifc, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("interface %s did not appear: %w", name, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
