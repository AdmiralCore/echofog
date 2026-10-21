package main

import (
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
)

func TestRoleFlag(t *testing.T) {
	t.Setenv("ECHOFOG_ROLE", "")
	if got := parseRunFlags(nil).config().Role; got != config.RoleClient {
		t.Errorf("default role = %q, want client", got)
	}
	if got := parseRunFlags([]string{"--role", "server"}).config(); !got.IsServer() {
		t.Errorf("--role server gave role %q", got.Role)
	}
	t.Setenv("ECHOFOG_ROLE", "server")
	if got := parseRunFlags(nil).config(); !got.IsServer() {
		t.Errorf("ECHOFOG_ROLE=server gave role %q", got.Role)
	}
	if got := parseRunFlags([]string{"--role", "client"}).config(); got.IsServer() {
		t.Error("the flag must win over the environment")
	}
}

func TestStatusShowsRole(t *testing.T) {
	server := Status{Role: config.RoleServer, Dropped: 7}
	if line := statsLine(server); !strings.Contains(line, "role=server dropped=7") {
		t.Errorf("stats line misses the role: %s", line)
	}
	if line := roleLine(server); !strings.HasPrefix(line, "server") {
		t.Errorf("status role line = %q", line)
	}
	client := Status{Role: config.RoleClient}
	if line := statsLine(client); strings.Contains(line, "role=") {
		t.Errorf("a client's stats line should stay as it was: %s", line)
	}
	if line := roleLine(Status{}); !strings.HasPrefix(line, "client") {
		t.Errorf("a status without a role comes from an older node, which was a client: %q", line)
	}
}
