// Command launchpad-scenarios runs the v2 scenarios of the launchpad (T15.1) in the model: the
// product's own code against a model of the network and of the censor, in one process, without
// containers. It writes scenarios.json (read by launchpad-gate -scenarios) and scenarios.md.
//
//	launchpad-scenarios -list
//	launchpad-scenarios -scenario v2-nat -runs 3 -out reports/scenarios
//	launchpad-scenarios -out reports/scenarios            # all seven, three runs each
//
// Exit codes: 0 all green, 1 a red row, 2 something was not measured (add -allow-unmeasured for a
// partial run), 3 bad arguments. The live mode of the same scenarios runs on the Docker stand with
// `go run ./testbed/runner -scenario <name>`; it is not part of this command.
package main

import (
	"os"
	"os/exec"
	"strings"

	"github.com/echofog/echofog-core/testbed/scenarios"
)

func main() {
	os.Exit(scenarios.Command(os.Args[1:], os.Stdout, os.Stderr, scenarios.Registry(), shortCommit))
}

func shortCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
