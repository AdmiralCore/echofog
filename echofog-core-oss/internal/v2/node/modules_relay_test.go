package node_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// relay/2 is offered only with the relay role on a node that is neither metered nor a phone
// (platforms.md §2: no relaying for others on a metered network, none on the mobile profile).
func TestRelayServedOnlyWhereThePlatformAllows(t *testing.T) {
	relayRole := func(c *cards.Config) { c.Role = objects.RoleServer; c.Flags |= objects.FlagRelay }
	cases := []struct {
		name string
		prof node.Profile
		want bool
	}{
		{"server with relay role", node.Profile{Platform: node.PlatformServer, Roles: []node.Role{node.RoleRelay}}, true},
		{"desktop with relay role", node.Profile{Platform: node.PlatformDesktop, Roles: []node.Role{node.RoleRelay}}, true},
		{"no relay role", node.Profile{Platform: node.PlatformServer}, false},
		{"metered", node.Profile{Platform: node.PlatformServer, Roles: []node.Role{node.RoleRelay}, Metered: true}, false},
		{"android", node.Profile{Platform: node.PlatformAndroid, Roles: []node.Role{node.RoleRelay}}, false},
		{"ios", node.Profile{Platform: node.PlatformIOS, Roles: []node.Role{node.RoleRelay}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newTestNodeFull(t, cards.Visible, false, nil, nil, relayRole,
				func(c *node.Config) { c.Profile = tc.prof })
			n.start(t)
			// The handler is there on every node (the client side answers INCOMING from its relays,
			// T6.2); only the roles that relay for others serve CONNECT and REGISTER.
			if !node.HasHandler(n.rt, service.Relay, 2) {
				t.Fatal("relay/2 has no handler: INCOMING from a relay could not be answered")
			}
			if got := node.RelaysForOthers(tc.prof); got != tc.want {
				t.Fatalf("relays for others = %v, want %v", got, tc.want)
			}
		})
	}
}
