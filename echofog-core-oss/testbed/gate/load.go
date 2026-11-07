package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

// decodeOne decodes one JSON value into v and refuses data after it.
func decodeOne(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("data after the first JSON value")
	}
	return nil
}

func validateRun(r *RunReport) error {
	if r.Schema != 2 {
		return fmt.Errorf("report schema %d: want 2", r.Schema)
	}
	if len(r.Detectors) == 0 {
		return errors.New("the report has no detectors")
	}
	for id, d := range r.Detectors {
		if math.IsNaN(d.FPR) || d.FPR < 0 || d.FPR > 1 {
			return fmt.Errorf("detector %s: fpr %v is not a rate", id, d.FPR)
		}
		if d.Recall != nil && (math.IsNaN(*d.Recall) || *d.Recall < 0 || *d.Recall > 1) {
			return fmt.Errorf("detector %s: recall %v is not a rate", id, *d.Recall)
		}
	}
	return nil
}

// LoadRunReport reads the runner's report.json. Fields the gate does not read are ignored; a
// report that is not schema 2, has no detectors or has a rate outside 0..1 is refused.
func LoadRunReport(r io.Reader) (*RunReport, error) {
	var rep RunReport
	if err := decodeOne(r, &rep); err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	if err := validateRun(&rep); err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	return &rep, nil
}

func validateSpeed(m map[string]float64) error {
	for c, v := range m {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return fmt.Errorf("carrier %q: %v Mbit/s is not a speed", c, v)
		}
	}
	return nil
}

// LoadBaseline reads a baseline file. It must carry the date and the commit it was signed with.
func LoadBaseline(r io.Reader) (*Baseline, error) {
	var b Baseline
	if err := decodeOne(r, &b); err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	if b.SignedAt.IsZero() || b.Commit == "" {
		return nil, errors.New("baseline: it is not signed with a date and a commit")
	}
	if err := validateRun(&b.Run); err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	if err := validateSpeed(b.Speed); err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	return &b, nil
}

// LoadBehaviour reads the report of launchpad-behaviour.
func LoadBehaviour(r io.Reader) (*behaviour.Report, error) {
	var b behaviour.Report
	if err := decodeOne(r, &b); err != nil {
		return nil, fmt.Errorf("behaviour report: %w", err)
	}
	if len(b.Freeze) == 0 && len(b.Dial) == 0 && len(b.Net) == 0 && len(b.PenaltyNever.Attempts) == 0 {
		return nil, errors.New("behaviour report: nothing in it")
	}
	return &b, nil
}

// LoadSpeed reads a speed file: carrier name to Mbit/s of `efv2 echo`.
func LoadSpeed(r io.Reader) (map[string]float64, error) {
	var m map[string]float64
	if err := decodeOne(r, &m); err != nil {
		return nil, fmt.Errorf("speed: %w", err)
	}
	if len(m) == 0 {
		return nil, errors.New("speed: no carrier")
	}
	if err := validateSpeed(m); err != nil {
		return nil, fmt.Errorf("speed: %w", err)
	}
	return m, nil
}

// Files names what a nightly run reads; empty means "not given".
type Files struct {
	Report, Behaviour, Baseline, Speed string
}

func open[T any](path string, load func(io.Reader) (T, error)) (T, error) {
	var zero T
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	return load(f)
}

// Nightly reads the files and evaluates them. The clock and the commit come from the caller in in;
// the rest of in is filled from the files. The report is required: no report, no gate.
func Nightly(f Files, in Input) (Result, error) {
	if f.Report == "" {
		return Result{}, errors.New("gate: no run report given")
	}
	var err error
	if in.Run, err = open(f.Report, LoadRunReport); err != nil {
		return Result{}, err
	}
	if f.Behaviour != "" {
		if in.Behaviour, err = open(f.Behaviour, LoadBehaviour); err != nil {
			return Result{}, err
		}
	}
	if f.Baseline != "" {
		if in.Baseline, err = open(f.Baseline, LoadBaseline); err != nil {
			return Result{}, err
		}
	}
	if f.Speed != "" {
		if in.Speed, err = open(f.Speed, LoadSpeed); err != nil {
			return Result{}, err
		}
	}
	return Evaluate(in), nil
}
