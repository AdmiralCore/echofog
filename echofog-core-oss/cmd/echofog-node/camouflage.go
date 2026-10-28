package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/internal/network/natcheck"
)

// camouflageCmd finds real sites in this server's hosting block that the node can imitate, the way
// the node does by itself, and prints the lines to put in deploy/.env. It needs no running node.
func camouflageCmd(args []string) int {
	f := flag.NewFlagSet("camouflage", flag.ExitOnError)
	ipFlag := f.String("ip", "", "this server's public IPv4 address (default: ECHOFOG_PUBLIC_ADDR, the interface address, or what STUN sees)")
	max := f.Int("max", 5, "stop after this many suitable sites")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), `Usage: echofog-node camouflage [flags]

Looks for real websites next to this server that the node can pretend to be: sites with a publicly
trusted certificate for a real domain that answer TLS 1.3 exactly like the node does. It probes the
server's own /24 first, then the /24s next to it, a few addresses at a time, and prints what it
finds with the two lines for deploy/.env. A public node runs the same search by itself; use this
when its log says it found no site, or to choose one yourself.

With Docker, in deploy/:
  docker compose run --rm node camouflage`)
		f.PrintDefaults()
	}
	_ = f.Parse(args)

	ip, how, err := publicIPv4(*ipFlag)
	if err != nil {
		fatal("%v", err)
	}
	cert, err := mimicry.GenerateCamouflageCert("")
	if err != nil {
		fatal("measuring the node's own TLS answer: %v", err)
	}
	ours, err := mimicry.OwnServerProfile(mimicry.BuildServerTLSConfig(cert))
	if err != nil {
		fatal("measuring the node's own TLS answer: %v", err)
	}

	cands := mimicry.WideCandidates(ip)
	fmt.Printf("Server address %s (%s). Probing up to %d addresses around it, this takes a few minutes.\n", ip, how, len(cands))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	last := time.Now()
	found := mimicry.FindNeighbors(ctx, cands, ours, *max, func(done, total int) {
		if time.Since(last) > 10*time.Second {
			last = time.Now()
			fmt.Fprintf(os.Stderr, "  %d of %d probed\n", done, total)
		}
	})
	if len(found) == 0 {
		fmt.Println(`
No suitable site found. The node then runs on UDP only, which works for most people but not on
networks that block UDP. A site of your own also works: point a domain at another small server in
the same datacenter, serve it over HTTPS with a real certificate (for example with Caddy), and use
that domain and address below.`)
		return 1
	}
	fmt.Println("\nSites the node can imitate (the first is the closest):")
	for _, nb := range found {
		fmt.Printf("  %-40s %s\n", nb.ServerName, nb.Addr)
	}
	fmt.Printf(`
Put these two lines in deploy/.env (the first site, or another from the list), then run
"docker compose up -d" and send people the new link from "echofog-node invite":

ECHOFOG_TLS_SERVER_NAME=%s
ECHOFOG_CAMOUFLAGE_ADDR=%s
`, found[0].ServerName, found[0].Addr)
	return 0
}

// publicIPv4 is this server's public IPv4 address and where it came from: the flag, then
// ECHOFOG_PUBLIC_ADDR, then the interface address when it is public, then what STUN servers see.
func publicIPv4(flagIP string) (netip.Addr, string, error) {
	public := func(s string) (netip.Addr, bool) {
		if h, _, err := net.SplitHostPort(s); err == nil {
			s = h
		}
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return netip.Addr{}, false
		}
		a = a.Unmap()
		return a, a.Is4() && a.IsGlobalUnicast() && !a.IsPrivate() && !netip.MustParsePrefix("100.64.0.0/10").Contains(a)
	}
	if flagIP != "" {
		if a, ok := public(flagIP); ok {
			return a, "--ip", nil
		}
		return netip.Addr{}, "", fmt.Errorf("--ip %q is not a public IPv4 address", flagIP)
	}
	for _, s := range strings.Split(os.Getenv("ECHOFOG_PUBLIC_ADDR"), ",") {
		if a, ok := public(s); ok {
			return a, "ECHOFOG_PUBLIC_ADDR", nil
		}
	}
	if c, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		local := c.LocalAddr().String()
		c.Close()
		if a, ok := public(local); ok {
			return a, "this server's interface", nil
		}
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return netip.Addr{}, "", err
	}
	defer conn.Close()
	obs, err := natcheck.QuerySTUN(context.Background(), conn, natcheck.DefaultSTUNServers, 3*time.Second)
	if err == nil {
		for _, o := range obs {
			if a, ok := public(o.Addr.Addr().String()); ok {
				return a, "seen by STUN servers", nil
			}
		}
	}
	return netip.Addr{}, "", fmt.Errorf("could not find this server's public IPv4 address; pass it with --ip")
}
