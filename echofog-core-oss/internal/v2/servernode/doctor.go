package servernode

import (
	"context"
	"net/netip"
	"time"
)

// Level of a doctor result.
type Level int

const (
	LevelOK Level = iota
	LevelInfo
	LevelWarn
	LevelFail
)

// CheckResult is one line of the doctor.
type CheckResult struct {
	ID    string
	Level Level
	Code  string
	Args  []any
}

// Text is the line for the operator.
func (r CheckResult) Text(lang string) string { return "" }

// DoctorDeps are the doctor's inputs. A nil probe means the check is skipped: nothing leaves the
// machine unless the operator asked for it.
type DoctorDeps struct {
	DataDir    string
	Config     *Config
	Now        func() time.Time
	Resolve    func(ctx context.Context, host string) ([]netip.Addr, error)
	FreeBytes  func(path string) (uint64, error)
	ListenUDP  func(addr string) error
	NodeStatus func(ctx context.Context) (*StatusInfo, error)
	Version    string
}

// RunDoctor runs the checks.
func RunDoctor(ctx context.Context, d DoctorDeps) []CheckResult { return nil }

// DoctorExit is the exit code: 1 when any check failed.
func DoctorExit(rs []CheckResult) int { return 0 }
