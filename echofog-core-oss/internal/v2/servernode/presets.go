package servernode

import "errors"

// Preset is a ready choice of roles for the setup wizard.
type Preset string

const (
	PresetRelay       Preset = "relay"
	PresetExitFree    Preset = "exit-free"
	PresetHomeEntry   Preset = "home-entry"
	PresetExitPaid    Preset = "exit-paid"
	PresetExitPrivate Preset = "exit-private"
)

// ErrPresetUnavailable: the preset needs something this build does not have yet.
var ErrPresetUnavailable = errors.New("servernode: preset is not available in this build")

// PresetOptions are the answers a preset needs.
type PresetOptions struct {
	Advertise string
	Port      int
	Domain    string
	SiteDir   string
}

// Note is a remark for the operator, a message code with a level ("info" or "warn").
type Note struct {
	Level string
	Code  string
}

// BuildPreset turns a preset and the answers into a configuration and the notes to show.
func BuildPreset(Preset, PresetOptions) (Config, []Note, error) {
	return Config{}, nil, errors.New("servernode: not implemented")
}

// IsFreeDDNS reports whether a domain belongs to a well-known free dynamic DNS service.
func IsFreeDDNS(string) bool { return false }

// RenderConfig is server.json with comments, ready to be edited by hand.
func RenderConfig(Config, string) ([]byte, error) {
	return nil, errors.New("servernode: not implemented")
}
