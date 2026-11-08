package scenarios

import "io"

// Command is launchpad-scenarios: it runs the scenarios in the model, writes scenarios.json (which
// launchpad-gate reads with -scenarios) and a table of scenario by mode by status. Exit codes: 0 all
// green, 1 a red row, 2 something was not measured (unless -allow-unmeasured), 3 bad arguments.
func Command(args []string, stdout, stderr io.Writer, reg []Scenario, commit func() string) int {
	return 99
}
