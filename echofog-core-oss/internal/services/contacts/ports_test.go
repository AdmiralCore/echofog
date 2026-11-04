package contacts

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	apitypes "github.com/echofog/echofog-core/pkg/api/types"
)

// Contacts keeps records in the database; the only thing it needs from the network is whether a
// peer is reachable. That comes through the Wire port, so the package itself must not import a
// transport: the v1 swarm adapter lives outside (internal/services/contactswire, T7.0).
func TestContactsDoesNotImportTransport(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain is not in PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		dep = strings.TrimSpace(dep)
		switch {
		case dep == "":
		case strings.Contains(dep, "/internal/p2p"):
			t.Errorf("contacts imports the v1 p2p network: %s", dep)
		case strings.HasPrefix(dep, "github.com/echofog/echofog-core/internal/proto"):
			t.Errorf("contacts imports internal/proto: %s", dep)
		case strings.Contains(dep, "/internal/network") && !strings.HasSuffix(dep, "/internal/network/net_types"):
			t.Errorf("contacts imports the v1 network layer: %s", dep)
		}
	}
}

// fakeWire stands in for the network: only the peers listed as reachable answer true.
type fakeWire struct {
	reachable map[apitypes.PeerID]bool
}

func (f fakeWire) Connected(id apitypes.PeerID) bool { return f.reachable[id] }

func TestOnlineAsksTheWirePort(t *testing.T) {
	here := apitypes.PeerID{7}
	svc := NewContactService(nil, fakeWire{reachable: map[apitypes.PeerID]bool{here: true}})

	if !svc.Online(here.String()) {
		t.Fatal("a reachable peer must count as online")
	}
	if svc.Online((apitypes.PeerID{8}).String()) {
		t.Fatal("a peer the wire does not list is offline")
	}
	if svc.Online("not a peer id") {
		t.Fatal("a malformed peer ID is offline, not an error")
	}
}

func TestOnlineWithoutAWire(t *testing.T) {
	svc := NewContactService(nil, nil)
	if svc.Online(strings.Repeat("00", 32)) {
		t.Fatal("a service built without a wire knows no reachable peer")
	}
}
