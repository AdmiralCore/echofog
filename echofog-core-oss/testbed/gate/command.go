package gate

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// MarshalText makes a status a word in gate.json.
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Command is the whole of the nightly command: flags in, files and an exit code out. It returns 0
// when nothing blocks and everything was measured, 1 when a metric is red (the release is
// blocked), 2 when something was not measured (unless -allow-unmeasured) or nothing was, 3 for bad
// arguments or unreadable files. The clock and the commit
// are the caller's, so a test sets both.
func Command(args []string, stdout, stderr io.Writer, now func() time.Time, commit func() string) int {
	fs := flag.NewFlagSet("launchpad-gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var f Files
	fs.StringVar(&f.Report, "report", "", "report.json of the run (required)")
	fs.StringVar(&f.Behaviour, "behaviour", "", "report of launchpad-behaviour")
	fs.StringVar(&f.Baseline, "baseline", "", "signed baseline for the regression check")
	fs.StringVar(&f.Speed, "speed", "", "JSON of carrier name to Mbit/s of `efv2 echo`")
	fs.StringVar(&f.Scenarios, "scenarios", "", "scenarios.json of launchpad-scenarios (T15.1)")
	out := fs.String("out", ".", "directory for gate.json and checklist.md")
	allow := fs.Bool("allow-unmeasured", false, "accept a partial run (exit 0 although something was not measured); an empty run still exits 2")
	sign := fs.String("write-baseline", "", "sign the report as a baseline and write it here, instead of judging it")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, "launchpad-gate:", err)
		return 3
	}
	if f.Report == "" {
		fmt.Fprintln(stderr, "launchpad-gate: -report is required")
		return 3
	}

	if *sign != "" {
		run, err := open(f.Report, LoadRunReport)
		if err != nil {
			fmt.Fprintln(stderr, "launchpad-gate:", err)
			return 3
		}
		var speed map[string]float64
		if f.Speed != "" {
			if speed, err = open(f.Speed, LoadSpeed); err != nil {
				fmt.Fprintln(stderr, "launchpad-gate:", err)
				return 3
			}
		}
		b := MakeBaseline(*run, speed, now(), commit())
		raw, err := json.MarshalIndent(b, "", "  ")
		if err == nil {
			err = os.WriteFile(*sign, append(raw, '\n'), 0o644)
		}
		if err != nil {
			fmt.Fprintln(stderr, "launchpad-gate:", err)
			return 3
		}
		fmt.Fprintf(stdout, "baseline signed %s at commit %s: %s\n", b.SignedAt.UTC().Format("2006-01-02"), b.Commit, *sign)
		return 0
	}

	res, err := Nightly(f, Input{Now: now(), Commit: commit()})
	if err != nil {
		fmt.Fprintln(stderr, "launchpad-gate:", err)
		return 3
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(stderr, "launchpad-gate:", err)
		return 3
	}
	raw, err := json.MarshalIndent(res, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(*out, "gate.json"), append(raw, '\n'), 0o644)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(*out, "checklist.md"), []byte(RenderChecklist(res)), 0o644)
	}
	if err != nil {
		fmt.Fprintln(stderr, "launchpad-gate:", err)
		return 3
	}

	fmt.Fprintf(stdout, "launchpad gate: %s (commit %s, %s)\n", res.Verdict, res.Commit, res.SignedAt.UTC().Format("2006-01-02"))
	missing := 0
	for _, r := range res.Rows {
		switch r.Status {
		case Red, Yellow:
			fmt.Fprintf(stdout, "  %-6s %s %s: %s (limit: %s) %s\n", r.Status, r.ID, r.Metric, r.Value, r.Limit, r.Note)
		case NotMeasured:
			missing++
		}
	}
	for _, c := range res.Changes {
		fmt.Fprintf(stdout, "  %-6s change %s: %s -> %s (%s)\n", c.Status, c.Key, c.Before, c.After, c.Why)
	}
	if missing > 0 {
		fmt.Fprintf(stderr, "launchpad-gate: %d metrics not measured in this run (see gate.json)\n", missing)
	}
	if res.Blocks {
		fmt.Fprintln(stdout, "the release is blocked")
	}
	return ExitCode(res, *allow)
}
