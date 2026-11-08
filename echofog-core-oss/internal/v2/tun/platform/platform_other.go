//go:build !windows

package platform

import (
	"context"
	"net"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/ks"
)

// HelperFlag as os.Args[1] makes the executable run RunHelper.
const HelperFlag = ks.HelperFlag

// Supported reports whether the kill switch has a driver here: not yet (T10.6c brings Linux).
func Supported() bool { return false }

// Drivers has no driver: every operation answers tun.ErrUnsupported.
func Drivers(exe string) *ks.Client {
	return ks.NewClient(ks.ClientConfig{
		Connect:   func(context.Context) (net.Conn, error) { return nil, tun.ErrUnsupported },
		Installed: func(context.Context) (bool, error) { return false, nil },
	})
}

// RunHelper has nothing to run.
func RunHelper(args []string) int { return 2 }

// Report is what RemoveAll did.
type Report struct{ Filters int }

// RemoveAll has nothing to remove.
func RemoveAll(ctx context.Context) (Report, error) { return Report{}, tun.ErrUnsupported }
