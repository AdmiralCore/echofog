//go:build !windows && !linux

package tunmode

func Supported() bool { return false }

func HelperMain(args []string) int { return 1 }

func launchElevated(exe string, args []string) error { return ErrUnsupported }

// DefaultInterfaceIndex is only needed on Windows.
func DefaultInterfaceIndex() (int, error) { return 0, nil }

// PhysicalInterfaceIndex is only needed on Windows.
func PhysicalInterfaceIndex() (int, error) { return 0, nil }
