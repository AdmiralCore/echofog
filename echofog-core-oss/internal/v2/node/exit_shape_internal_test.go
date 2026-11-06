package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/shape"
)

// З2 (T10.1 acceptance): H1 padding on the exit must come from the exit address profile and must
// never be silently off just because the application left a field unset.
func TestExitShapeProfile(t *testing.T) {
	udp, _ := shape.Builtin(shape.IDDefaultUDP)
	tls, _ := shape.Builtin(shape.IDDefaultTLS)

	t.Run("explicit exit profile wins", func(t *testing.T) {
		got := exitShape(Config{ExitShapeProfile: tls, ShapeProfile: udp})
		if got == nil || got.ID != shape.IDDefaultTLS {
			t.Fatalf("profile = %+v, want default-tls", got)
		}
	})
	t.Run("profile of the listener address", func(t *testing.T) {
		got := exitShape(Config{ListenerSources: []ListenerSource{Noise3Listener{Addr: ":1", Profile: tls}}})
		if got == nil || got.ID != shape.IDDefaultTLS {
			t.Fatalf("profile = %+v, want the listener's default-tls", got)
		}
	})
	t.Run("never silently off", func(t *testing.T) {
		got := exitShape(Config{})
		if got == nil || (got.Pad == nil && len(got.Buckets) == 0) {
			t.Fatalf("profile = %+v: padding is off by default", got)
		}
	})
	t.Run("none is explicit", func(t *testing.T) {
		none, _ := shape.Builtin(shape.IDNone)
		got := exitShape(Config{ExitShapeProfile: none})
		if got == nil || got.ID != shape.IDNone {
			t.Fatalf("profile = %+v, want none", got)
		}
	})
}
