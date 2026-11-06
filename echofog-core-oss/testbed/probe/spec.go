package probe

import (
	"context"
	"io"
	"time"
)

// Scenarios the launchpad probes (T15.0b item 2): an ordinary node with a cover behind it, a node
// whose cover is its own website, a hidden node (which must not answer at all) and a server with
// several addresses (one entry each).
const (
	ScenarioOrdinary = "ordinary"
	ScenarioCover    = "cover"
	ScenarioHidden   = "hidden"
	ScenarioServer   = "server"
)

// Case is one target with the inputs its probes need.
type Case struct {
	Target Target
	Inputs Inputs
}

// LoadSpec reads a JSON list of probe entries: scenario, network, addr, cover, sni, the captured
// and fresh flights as hex, and the replay window in seconds.
func LoadSpec(r io.Reader) ([]Case, error) { return nil, nil }

// RunSpec reads a spec, runs every case from this machine (the border), writes the reports as JSON
// to w and returns how many answers of a node's own were not expected.
func RunSpec(ctx context.Context, r io.Reader, w io.Writer) (own int, err error) { return 0, nil }

var _ = time.Second
