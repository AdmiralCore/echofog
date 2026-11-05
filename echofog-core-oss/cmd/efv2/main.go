// Command efv2 is the laboratory CLI of EchoFog v2: it creates a profile, runs a node, prints
// its card, dials a card, echoes a stream and datagrams and shows the node status.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "efv2:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "card":
		return cmdCard(args[1:])
	case "link":
		return cmdLink(args[1:])
	case "dial":
		return cmdDial(args[1:])
	case "echo":
		return cmdEcho(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// parseInterspersed accepts the positional argument before or after the flags. Every flag of
// this CLI takes a value except -h/--help, so a simple two-pass split is enough.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			flags = append(flags, a)
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			if !strings.Contains(a, "=") {
				if i+1 >= len(args) {
					return fmt.Errorf("flag %s needs a value", a)
				}
				i++
				flags = append(flags, args[i])
			}
		default:
			pos = append(pos, a)
		}
	}
	return fs.Parse(append(flags, pos...))
}

func usage(w *os.File) {
	fmt.Fprint(w, `efv2 — EchoFog v2 laboratory CLI
usage:
  efv2 init   --data DIR --profile server|desktop|android
  efv2 run    --data DIR [--listen noise3=HOST:PORT[,reality2-quic=HOST:PORT --cover HOST:PORT]]
  efv2 card   --data DIR [--scope public|peer] [--listen ...]
  efv2 dial   --data DIR <card.json>
  efv2 echo   --data DIR <device-id> [--card card.json] [--bytes N] [--datagrams N]
  efv2 status --data DIR

init prints the recovery phrase once. The data directory holds the v2 store, the identity file and
the card/status snapshots the other subcommands read while a node is running.

reality2-quic listeners wait for T2.6b (the reproducible cover); this build serves noise3.
`)
}
