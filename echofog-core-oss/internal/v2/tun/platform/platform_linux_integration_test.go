//go:build linux && !android && kslinux

// The integration test of the Linux kill switch. It installs real nftables rules and changes the
// DNS settings of the machine, so it runs only when asked to:
//
//	EF_KS_INTEGRATION=1 go test -tags kslinux ./internal/v2/tun/platform -run Integration -v
//
// as root, and only in a WSL instance or a virtual machine: the rules cut the network of the whole
// computer. The test removes the table, the interface and the DNS settings in Cleanup even when a
// step fails.
package platform

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"golang.org/x/sys/unix"
)

func integrationGate(t *testing.T) {
	t.Helper()
	if os.Getenv("EF_KS_INTEGRATION") != "1" {
		t.Skip("set EF_KS_INTEGRATION=1 and run it in WSL or a virtual machine, as root")
	}
	if os.Geteuid() != 0 {
		t.Skip("root is needed")
	}
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func cleanupIntegration(t *testing.T) {
	t.Helper()
	_ = exec.Command("nft", "delete", "table", "inet", TableName).Run()
	_ = exec.Command("ip", "link", "del", tunnelName).Run()
	_ = exec.Command("resolvectl", "revert", "eth0").Run()
	_ = os.Remove(dnsStatePath())
	_ = os.Remove(markerPath())
}

func tableExists(t *testing.T) bool {
	t.Helper()
	_, err := exec.Command("nft", "list", "table", "inet", TableName).CombinedOutput()
	return err == nil
}

func TestLinuxKillSwitchIntegration(t *testing.T) {
	integrationGate(t)

	ready := os.Getenv("EF_KS_READY")
	if ready != "" {
		runIntegrationChild(t, ready)
		return
	}

	if tableExists(t) {
		t.Fatal("a table of the kill switch already stands: run `sudo efv2 killswitch remove` first")
	}
	if _, err := net.InterfaceByName(tunnelName); err == nil {
		t.Fatalf("%s exists: a tunnel is already running", tunnelName)
	}
	t.Cleanup(func() { cleanupIntegration(t) })

	// A normal desktop link: eth0 holds a resolver and is the default route. WSL keeps its DNS in
	// the global section, so the test gives eth0 a per-link one first.
	sh(t, "resolvectl", "dns", "eth0", "10.255.255.254")
	sh(t, "resolvectl", "domain", "eth0", "~.")
	sh(t, "resolvectl", "default-route", "eth0", "yes")
	before := linkOf(t, resolveStatus(t), "eth0")

	dir := t.TempDir()
	readyFile := dir + "/ready"
	probe := net.ParseIP("203.0.113.10")
	plan := tun.Plan{
		Endpoints:  []tun.Endpoint{{Addr: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.10"), 4444), Proto: tun.ProtoUDP}},
		TunnelPool: tun.DefaultTunnelPool,
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/plan", raw, 0o600); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestLinuxKillSwitchIntegration", "-test.v")
	cmd.Env = append(os.Environ(),
		"EF_KS_INTEGRATION=1", "EF_KS_READY="+readyFile, "EF_KS_PLAN="+dir+"/plan")
	var childOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childOut, &childOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var childErr error
	done := make(chan struct{})
	go func() { childErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("the helper ended before it was ready: %v\n%s", childErr, childOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper did not start in time\n%s", childOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The filters and the DNS protection are there.
	if !tableExists(t) {
		t.Fatal("no table after the helper said it was ready")
	}
	if _, err := os.Stat(markerPath()); err != nil {
		t.Fatalf("no marker: %v", err)
	}
	if linkOf(t, resolveStatus(t), "eth0").DefaultRoute {
		t.Fatal("eth0 is still a default route: DNS may leave the tunnel")
	}
	if !linkOf(t, resolveStatus(t), tunnelName).DefaultRoute {
		t.Fatalf("%s is not the default route of the resolver", tunnelName)
	}

	// kill -9: the rules of the kernel stay, exactly what the kill switch promises.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	time.Sleep(300 * time.Millisecond) // the kernel drops the dummy device of the killed child
	if !tableExists(t) {
		t.Fatal("the rules are gone after the helper was killed")
	}
	if _, err := os.Stat(markerPath()); err != nil {
		t.Fatalf("the marker is gone after the helper was killed: %v", err)
	}

	// DNS does not go past the tunnel: a query to the outside on port 53 does not reach eth0, while
	// a permitted datagram does. The second count proves the capture works.
	got := captureProbes(t, "eth0", probe, []uint16{4444, 53})
	if got[4444] == 0 {
		t.Fatalf("the capture saw no permitted datagram: %v (the capture itself is broken)", got)
	}
	if got[53] != 0 {
		t.Fatalf("%d DNS datagrams left on eth0 though the filters stand", got[53])
	}

	// The manual removal: the table goes, DNS goes back as it was.
	rep, err := RemoveAll(context.Background())
	if err != nil {
		t.Fatalf("RemoveAll = %v", err)
	}
	if rep.Filters == 0 {
		t.Fatal("RemoveAll removed no rules")
	}
	if tableExists(t) {
		t.Fatal("the table stands after RemoveAll")
	}
	if _, err := os.Stat(dnsStatePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the DNS snapshot is still there: %v", err)
	}
	after := linkOf(t, resolveStatus(t), "eth0")
	if !after.DefaultRoute {
		t.Fatal("eth0 is not a default route again")
	}
	if strings.Join(after.DNS, " ") != strings.Join(before.DNS, " ") {
		t.Fatalf("eth0 DNS %v, want %v", after.DNS, before.DNS)
	}
}

// runIntegrationChild installs the real rules and DNS protection, then waits to be killed.
func runIntegrationChild(t *testing.T, ready string) {
	raw, err := os.ReadFile(os.Getenv("EF_KS_PLAN"))
	if err != nil {
		t.Fatal(err)
	}
	var plan tun.Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	rules, err := tun.BuildRules(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := newFirewall().Install(context.Background(), rules); err != nil {
		t.Fatal(err)
	}
	// A real Tunnel.Up creates the device and tun2socks; a dummy device is enough for the firewall
	// and the DNS part of this test.
	sh(t, "ip", "link", "add", tunnelName, "type", "dummy")
	sh(t, "ip", "addr", "add", "198.18.0.1/15", "dev", tunnelName)
	sh(t, "ip", "link", "set", tunnelName, "up")
	if err := newDNS().Protect(context.Background(), tun.DNSConfig{
		Tunnel: tunnelName, Resolver: netip.MustParseAddr("198.18.0.2"), DisableSmartNameResolution: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Minute)
}

// captureProbes sends datagrams to the probe address on every port and counts what reaches the
// interface. A raw AF_PACKET socket sees the frames before any host-side NAT.
func captureProbes(t *testing.T, ifname string, dst net.IP, ports []uint16) map[uint16]int {
	t.Helper()
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifc.Index}); err != nil {
		t.Fatal(err)
	}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 100000})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counts := map[uint16]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frame := make([]byte, 1<<16)
		for ctx.Err() == nil {
			n, _, err := unix.Recvfrom(fd, frame, 0)
			if err != nil {
				continue
			}
			port, ok := udpDstPort(frame[:n], dst)
			if !ok {
				continue
			}
			for _, p := range ports {
				if p == port {
					mu.Lock()
					counts[p]++
					mu.Unlock()
				}
			}
		}
	}()
	// Several sends: one lost datagram does not decide the test.
	for i := 0; i < 5; i++ {
		for _, p := range ports {
			conn, err := net.Dial("udp4", net.JoinHostPort(dst.String(), strconv.Itoa(int(p))))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = conn.Write([]byte("echofog-killswitch-probe"))
			conn.Close()
			time.Sleep(20 * time.Millisecond)
		}
	}
	time.Sleep(500 * time.Millisecond)
	cancel()
	wg.Wait()
	return counts
}

func udpDstPort(frame []byte, dst net.IP) (uint16, bool) {
	if len(frame) < 14+20+8 || frame[12] != 0x08 || frame[13] != 0x00 {
		return 0, false
	}
	ip := frame[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl+8 || ip[9] != 17 {
		return 0, false
	}
	if !net.IP(ip[16:20]).Equal(dst.To4()) {
		return 0, false
	}
	udp := ip[ihl:]
	return binary.BigEndian.Uint16(udp[2:4]), true
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func resolveStatus(t *testing.T) []linkState {
	t.Helper()
	out, err := exec.Command("resolvectl", "status").CombinedOutput()
	if err != nil {
		t.Fatalf("resolvectl status: %v: %s", err, out)
	}
	links := parseStatus(string(out))
	dns, err := exec.Command("resolvectl", "dns").CombinedOutput()
	if err != nil {
		t.Fatalf("resolvectl dns: %v: %s", err, dns)
	}
	servers := parseLinkValues(string(dns))
	for i := range links {
		links[i].DNS = servers[links[i].Name]
	}
	return links
}

func linkOf(t *testing.T, links []linkState, name string) linkState {
	t.Helper()
	for _, l := range links {
		if l.Name == name {
			return l
		}
	}
	return linkState{Name: name}
}
