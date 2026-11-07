package gate

import (
	"errors"
	"io"

	"github.com/echofog/echofog-core/testbed/behaviour"
)

var errTodo = errors.New("not implemented")

// LoadRunReport reads the runner's report.json.
func LoadRunReport(r io.Reader) (*RunReport, error) { return nil, errTodo }

// LoadBaseline reads a baseline file.
func LoadBaseline(r io.Reader) (*Baseline, error) { return nil, errTodo }

// LoadBehaviour reads the report of launchpad-behaviour.
func LoadBehaviour(r io.Reader) (*behaviour.Report, error) { return nil, errTodo }

// LoadSpeed reads a speed file: carrier name to Mbit/s.
func LoadSpeed(r io.Reader) (map[string]float64, error) { return nil, errTodo }

// Files names what a nightly run reads; empty means "not given".
type Files struct {
	Report, Behaviour, Baseline, Speed string
}

// Nightly reads the files and evaluates them. The clock and the commit come from the caller.
func Nightly(f Files, in Input) (Result, error) { return Result{}, errTodo }
