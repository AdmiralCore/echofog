package main

// The live mode of the T15.1 scenarios (v2-nat, v2-paths, v2-hidden, v2-messaging, v2-call, v2-exit).
//
// What is ready: the compose services (profile "v2") and the efv2 role of the entrypoint, checked
// statically. What is not: the efv2 CLI has no command for chat, calls, the exit, hidden access or
// a second path yet (testbed/scenarios/live.go lists them per scenario), so the runner cannot drive
// these scenarios in containers. It says so, starts nothing, and exits 4: prepared, not runnable.
// The same scenarios run in the model with `go run ./testbed/scenarios/cmd/launchpad-scenarios`.
// Three green live runs in a row are the task of the audit phase, not of this change.

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/echofog/echofog-core/testbed/scenarios"
)

var v2LiveScenarios = []string{"v2-nat", "v2-paths", "v2-hidden", "v2-messaging", "v2-call", "v2-exit"}

func isV2LiveScenario(s string) bool { return slices.Contains(v2LiveScenarios, s) }

// runV2Live is the live entry of a T15.1 scenario: exit 4 while the efv2 CLI cannot drive it, 3 for a
// name that is not one.
func runV2Live(name string, w io.Writer) int {
	for _, s := range scenarios.Registry() {
		if s.Name != name {
			continue
		}
		fmt.Fprintf(w, "%s: the live mode is prepared and not runnable yet (not verified).\n", name)
		fmt.Fprintf(w, "  compose services (profile v2): %s\n", strings.Join(s.Live.Services, ", "))
		fmt.Fprintln(w, "  what the efv2 CLI or the stand still needs:")
		for _, n := range s.Live.Needs {
			fmt.Fprintf(w, "    - %s\n", n)
		}
		fmt.Fprintf(w, "  the model mode runs now: go run ./testbed/scenarios/cmd/launchpad-scenarios -scenario %s\n", name)
		fmt.Fprintln(w, "  three green live runs in a row are for the audit phase; nothing was started.")
		return 4
	}
	fmt.Fprintf(w, "unknown live scenario %q (known: %s)\n", name, strings.Join(v2LiveScenarios, ", "))
	return 3
}
