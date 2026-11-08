package scenarios

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/echofog/echofog-core/testbed/gate"
)

const liveNotVerified = "not verified: the live stand is not run before the audit phase (architect's decision)"

// Command is launchpad-scenarios: it runs the scenarios in the model, writes scenarios.json (which
// launchpad-gate reads with -scenarios) and scenarios.md, a table of scenario by mode by status.
// Exit codes: 0 all green, 1 a red row, 2 something was not measured (unless -allow-unmeasured),
// 3 bad arguments or unwritable files. reg is the registry (Registry() in production).
func Command(args []string, stdout, stderr io.Writer, reg []Scenario, commit func() string) int {
	fs := flag.NewFlagSet("launchpad-scenarios", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	which := fs.String("scenario", "all", "a scenario name, or all")
	runs := fs.Int("runs", gate.ScenariosNeeded, "runs in a row; the task asks for three green")
	seed := fs.Int64("seed", 1, "seed of the first run; run i uses seed+i")
	short := fs.Bool("short", false, "smaller transfers and fewer cases")
	out := fs.String("out", ".", "directory for scenarios.json and scenarios.md")
	list := fs.Bool("list", false, "list the scenarios and their live commands and exit")
	allow := fs.Bool("allow-unmeasured", false, "accept a run in which something was not measured (exit 0)")
	timeout := fs.Duration("timeout", 30*time.Minute, "time for the whole run")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, "launchpad-scenarios:", err)
		return 3
	}
	if *runs < 1 {
		fmt.Fprintf(stderr, "launchpad-scenarios: -runs %d: want at least 1\n", *runs)
		return 3
	}
	if *list {
		for _, s := range reg {
			fmt.Fprintf(stdout, "%-14s %s\n    live: %s\n    services: %s\n", s.Name, s.Title, s.Live.Command, strings.Join(s.Live.Services, " "))
			for _, n := range s.Live.Needs {
				fmt.Fprintf(stdout, "    needs: %s\n", n)
			}
		}
		return 0
	}
	var picked []Scenario
	for _, s := range reg {
		if *which == "all" || *which == s.Name {
			picked = append(picked, s)
		}
	}
	if len(picked) == 0 {
		fmt.Fprintf(stderr, "launchpad-scenarios: no scenario %q\n", *which)
		return 3
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(stderr, "launchpad-scenarios:", err)
		return 3
	}
	scratch, err := os.MkdirTemp("", "launchpad-scenarios-")
	if err != nil {
		fmt.Fprintln(stderr, "launchpad-scenarios:", err)
		return 3
	}
	defer func() { _ = removeTree(scratch) }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rep := gate.ScenarioReport{Schema: 1, Commit: commit()}
	var md strings.Builder
	fmt.Fprintf(&md, "# Scenarios T15.1\n\nCommit %s. Model: the product's own code against a model of the network, in one process. Live: the Docker stand.\n\n", rep.Commit)
	md.WriteString("| scenario | mode | runs | status | live |\n|---|---|---|---|---|\n")
	code := 0
	var detail strings.Builder
	for _, s := range picked {
		fmt.Fprintf(stdout, "%s: %s\n", s.Name, s.Title)
		res, done := RunMany(ctx, s, Env{Seed: *seed, Short: *short, Dir: scratch}, *runs)
		rep.Scenarios = append(rep.Scenarios, res)
		worst := gate.Green
		for _, r := range res.Rows {
			if rank(r.Status) > rank(worst) {
				worst = r.Status
			}
		}
		if len(res.Rows) == 0 || (worst == gate.Green && res.Runs < *runs) {
			worst = gate.NotMeasured
		}
		fmt.Fprintf(stdout, "  model: %s (%d of %d runs", worst, res.Runs, *runs)
		if n := len(done); n > 0 {
			fmt.Fprintf(stdout, ", last %s", done[n-1].Elapsed.Round(100*time.Millisecond))
		}
		fmt.Fprintln(stdout, ")")
		for _, r := range res.Rows {
			if r.Status != gate.Green && r.Status != gate.Info {
				fmt.Fprintf(stdout, "    %-12s %s: %s (limit: %s) %s\n", r.Status, r.ID, r.Value, r.Limit, r.Note)
			}
		}
		switch worst {
		case gate.Red:
			code = 1
		case gate.NotMeasured, gate.Yellow:
			if code == 0 && !*allow {
				code = 2
			}
		}
		fmt.Fprintf(&md, "| %s | model | %d | %s | %s |\n", s.Name, res.Runs, worst, liveNotVerified)
		fmt.Fprintf(&detail, "\n## %s\n\n%s\n\nLive: `%s`; compose services: %s.\n\n", s.Name, s.Title, s.Live.Command, strings.Join(s.Live.Services, ", "))
		detail.WriteString("| row | value | limit | status |\n|---|---|---|---|\n")
		for _, r := range res.Rows {
			fmt.Fprintf(&detail, "| %s | %s | %s | %s |\n", r.ID, escape(r.Value), escape(r.Limit), r.Status)
		}
		for _, f := range res.Findings {
			fmt.Fprintf(&detail, "\nFinding: %s\n", f)
		}
		for _, n := range res.NotChecked {
			fmt.Fprintf(&detail, "\nNot checked: %s\n", n)
		}
		if len(s.Live.Needs) > 0 {
			detail.WriteString("\nThe live mode needs:\n")
			for _, n := range s.Live.Needs {
				fmt.Fprintf(&detail, "- %s\n", n)
			}
		}
	}
	md.WriteString(detail.String())

	raw, err := json.MarshalIndent(rep, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(*out, "scenarios.json"), append(raw, '\n'), 0o644)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(*out, "scenarios.md"), []byte(md.String()), 0o644)
	}
	if err != nil {
		fmt.Fprintln(stderr, "launchpad-scenarios:", err)
		return 3
	}
	fmt.Fprintf(stdout, "written: %s\n", filepath.Join(*out, "scenarios.json"))
	return code
}

func escape(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", "/"), "\n", " ") }
