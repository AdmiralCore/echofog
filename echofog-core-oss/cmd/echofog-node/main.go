// Command echofog-node runs a headless EchoFog node: a relay and DHT member that can also share its
// Internet connection as a paid or free exit. It is what you run on a server.
//
//	echofog-node init                 create a new identity and show its recovery phrase
//	echofog-node restore              restore an identity from its recovery phrase
//	echofog-node run [flags]          start the node (the default command)
//	echofog-node status               what a running node does: peers, relayed traffic, earnings
//	echofog-node wallet               the TON wallet of a running node
//	echofog-node invite               the invitation link of a running node
//	echofog-node version
package main

import (
	"fmt"
	"os"
	"strings"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const usage = `EchoFog node %s

Usage:
  echofog-node <command> [flags]

Commands:
  init      create a new identity and print its recovery phrase
  restore   restore an identity from its recovery phrase (asks for it, or --mnemonic-file)
  run       start the node: DHT, relay for others, optionally a public exit (default command)
  status    show what the running node does: peers, relayed traffic, exit earnings, wallet
  wallet    show the TON wallet address and balance of the running node
  invite    print the invitation link of the running node (--rotate, --list, --remove)
  version   print the version

Run "echofog-node <command> -h" for the flags of a command. Every flag can also be set with an
environment variable: --exit-price is ECHOFOG_EXIT_PRICE, --data-dir is ECHOFOG_DATA_DIR, and so on.
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == api.TunHelperFlag {
		os.Exit(api.RunTunHelper(os.Args[2:]))
	}
	cmd, args := "run", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		os.Exit(runCmd(args))
	case "init":
		os.Exit(initCmd(args))
	case "restore":
		os.Exit(restoreCmd(args))
	case "status":
		os.Exit(statusCmd(args, false))
	case "wallet":
		os.Exit(statusCmd(args, true))
	case "invite":
		os.Exit(inviteCmd(args))
	case "version", "--version", "-version":
		fmt.Println("echofog-node", version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
