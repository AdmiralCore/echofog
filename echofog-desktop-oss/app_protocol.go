package main

import (
	"os"
	"strconv"
	"strings"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// The protocol the node runs: 1 (the default until T15.5) or 2. The choice saved in the settings applies
// from the next start; the launch flag --protocol 2 (or --protocol=2) chooses the mode of one run and
// saves nothing.
const protocolFlag = "--protocol"

// takeProtocolFlag removes --protocol N / --protocol=N from the arguments after the program name and
// returns N (0: no flag, or a value that is not 1 or 2).
func takeProtocolFlag(args []string) ([]string, int) {
	kept := make([]string, 0, len(args))
	protocol := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if i > 0 && a == protocolFlag && i+1 < len(args) {
			protocol = parseProtocol(args[i+1])
			i++
			continue
		}
		if i > 0 && strings.HasPrefix(a, protocolFlag+"=") {
			protocol = parseProtocol(strings.TrimPrefix(a, protocolFlag+"="))
			continue
		}
		kept = append(kept, a)
	}
	return kept, protocol
}

func parseProtocol(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || (n != 1 && n != 2) {
		return 0
	}
	return n
}

// applyProtocolFlag reads the flag from the arguments and makes it the mode of this run.
func applyProtocolFlag() {
	args, p := takeProtocolFlag(os.Args)
	os.Args = args
	api.ForceProtocol(p)
}

// ProtocolDTO is what the settings screen shows: the protocol this run started with and the one saved for
// the next start.
type ProtocolDTO struct {
	Running int `json:"running"`
	Next    int `json:"next"`
}

// GetProtocol returns the protocol of this run and the one saved for the next start.
func (a *App) GetProtocol() (ProtocolDTO, error) {
	n := a.getAPI()
	if n == nil {
		return ProtocolDTO{}, errNodeNotStarted
	}
	return ProtocolDTO{Running: n.GetProtocol(), Next: n.SavedProtocol()}, nil
}

// SetProtocol saves the protocol for the next start; the running node is not touched.
func (a *App) SetProtocol(protocol int) error {
	n := a.getAPI()
	if n == nil {
		return errNodeNotStarted
	}
	return n.SetProtocol(protocol)
}
