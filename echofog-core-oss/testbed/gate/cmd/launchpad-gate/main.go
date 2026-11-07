// Command launchpad-gate is the nightly gate of the launchpad (T15.0e): it reads the report.json of
// a run, the report of launchpad-behaviour and a signed baseline, puts every metric in green,
// yellow or red, writes gate.json and the M4 checklist, and exits 1 when anything is red.
//
//	launchpad-gate -report testbed/out/<run>/report.json -behaviour behaviour.json \
//	    -baseline testbed/baseline.json -speed speed.json -out reports/gate
//	launchpad-gate -report report.json -speed speed.json -write-baseline testbed/baseline.json
//
// Exit codes: 0 nothing blocks, 1 red (the release is blocked), 2 not measured under -strict,
// 3 bad arguments or unreadable files.
package main

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/echofog/echofog-core/testbed/gate"
)

func main() {
	os.Exit(gate.Command(os.Args[1:], os.Stdout, os.Stderr, time.Now, shortCommit))
}

func shortCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
