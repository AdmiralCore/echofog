// Command launchpad-probe runs the active probes D11–D15 of a spec file from the machine it is on.
// The runner copies it into a gateway container (gw_b), so the probes come from the border and not
// from a node. It prints the reports as JSON and exits 1 when a node gave an answer of its own that
// nobody expected.
//
//	launchpad-probe -spec /tmp/probe.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/echofog/echofog-core/testbed/probe"
)

func main() {
	spec := flag.String("spec", "", "path of the JSON spec (see probe.LoadSpec)")
	allowPublic := flag.Bool("allow-public", false, "let the spec name public addresses and host names (default: lab, private and loopback only)")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up after this long")
	flag.Parse()
	if *spec == "" {
		fmt.Fprintln(os.Stderr, "launchpad-probe: -spec is required")
		os.Exit(2)
	}
	f, err := os.Open(*spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "launchpad-probe:", err)
		os.Exit(2)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	own, err := probe.RunSpecWith(ctx, f, os.Stdout, probe.SpecOptions{AllowPublic: *allowPublic})
	if err != nil {
		fmt.Fprintln(os.Stderr, "launchpad-probe:", err)
		os.Exit(2)
	}
	if own > 0 {
		fmt.Fprintf(os.Stderr, "launchpad-probe: %d answers of the node's own\n", own)
		os.Exit(1)
	}
}
