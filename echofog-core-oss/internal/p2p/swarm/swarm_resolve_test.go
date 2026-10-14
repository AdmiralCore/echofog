package swarm

import (
	"context"
	"testing"
)

func TestResolveHostPort(t *testing.T) {
	ctx := context.Background()
	if got := resolveHostPort(ctx, "localhost:4242"); got != "127.0.0.1:4242" && got != "[::1]:4242" {
		t.Fatalf("localhost resolved to %q", got)
	}
	if got := resolveHostPort(ctx, "203.0.113.5:4242"); got != "203.0.113.5:4242" {
		t.Fatalf("an IP changed: %q", got)
	}
	if got := resolveHostPort(ctx, "no-such-host.invalid:1"); got != "no-such-host.invalid:1" {
		t.Fatalf("an unknown name changed: %q", got)
	}
}
