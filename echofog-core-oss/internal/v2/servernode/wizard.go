package servernode

import (
	"errors"
	"io"
)

// ErrWizardInput: the input ended or stayed invalid.
var ErrWizardInput = errors.New("servernode: setup input ended or was invalid")

// WizardDefaults are the answers Enter gives.
type WizardDefaults struct {
	Lang      string
	Advertise string
	Port      int
	DataDir   string
}

// WizardResult is what the wizard decided.
type WizardResult struct {
	Lang    string
	Preset  Preset
	Opts    PresetOptions
	Config  Config
	Notes   []Note
	DataDir string
	Start   bool
}

// RunWizard asks the questions of the setup, one line each; Enter takes the default.
func RunWizard(in io.Reader, out io.Writer, d WizardDefaults) (*WizardResult, error) {
	return nil, errors.New("servernode: not implemented")
}
