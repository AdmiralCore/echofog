// Command launchpad-behaviour runs the behaviour, network and resilience module of the launchpad
// (T15.0c) and prints its report as JSON. It needs no Docker. The full run is the whole network
// matrix and a soak of -soak; -short is the subset the unit tests use.
//
//	launchpad-behaviour -short
//	launchpad-behaviour -soak 2h -out report.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

func main() {
	short := flag.Bool("short", false, "a representative subset of the matrix and a few seconds of soak")
	soak := flag.Duration("soak", time.Minute, "soak duration of a full run (2h for the criterion of the task)")
	out := flag.String("out", "", "write the report here instead of stdout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *soak+30*time.Minute)
	defer cancel()
	rep := behaviour.RunAll(ctx, behaviour.Options{Short: *short, Soak: *soak})

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "launchpad-behaviour:", err)
			os.Exit(2)
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(os.Stderr, "launchpad-behaviour:", err)
		os.Exit(2)
	}
	if len(rep.Findings) > 0 {
		fmt.Fprintf(os.Stderr, "launchpad-behaviour: %d findings, %d things not checked\n", len(rep.Findings), len(rep.NotChecked))
	}
}
