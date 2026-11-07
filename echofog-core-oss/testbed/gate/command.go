package gate

import (
	"io"
	"time"
)

// Command is the whole of the nightly command: flags in, files and an exit code out.
func Command(args []string, stdout, stderr io.Writer, now func() time.Time, commit func() string) int {
	return -1
}

// MarshalText makes a status readable in gate.json.
func (s Status) MarshalText() ([]byte, error) { return nil, nil }
