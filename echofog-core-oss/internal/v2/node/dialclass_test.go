package node_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// The journal names why an entry gave nothing, by class only. Each class the field asks for has a
// representative error (V2-FIELD-1).
func TestDialFailureClasses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no domestic entry", fmt.Errorf("dial: %w", node.ErrNoDomesticEntry), node.FailPolicy},
		{"route unsupported", node.ErrRouteUnsupported, node.FailPolicy},
		{"background", node.ErrBackground, node.FailBackground},
		{"proxy config", node.ErrEgressConfig, node.FailProxy},
		{"proxy network", node.ErrEgressNetwork, node.FailProxy},
		{"bad bootstrap target", fmt.Errorf("%w: suite 9", node.ErrBootstrapTarget), node.FailBadEntry},
		{"no peer card", node.ErrNoPeerCard, node.FailBadEntry},
		{"silent close", fmt.Errorf("session: %w", session.ErrSilent), node.FailNoAnswer},
		{"no route", node.ErrNoRoute, node.FailNoAnswer},
		{"deadline", fmt.Errorf("carrier: %w", context.DeadlineExceeded), node.FailTimeout},
		{"cancelled", context.Canceled, node.FailTimeout},
		{"unknown", errors.New("secret address 203.0.113.7"), node.FailOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := node.DialFailureClass(tc.err); got != tc.want {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
		})
	}
	if got := node.DialFailureClass(nil); got != "" {
		t.Fatalf("nil has class %q", got)
	}
}
