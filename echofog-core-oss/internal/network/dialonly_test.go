package network

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Anonymous through the own relay: a dial goes only to the allowed hosts or the local network; an
// empty list lets nothing out; nil lifts the rule.
func TestDialOnly(t *testing.T) {
	t.Cleanup(func() { SetDialOnly(nil) })
	ctx := context.Background()
	assert.False(t, DialRefused(ctx, "203.0.113.5:443"), "no rule: anywhere")

	SetDialOnly([]string{"198.51.100.7", "[2001:db8::1]"})
	assert.False(t, DialRefused(ctx, "198.51.100.7:443"))
	assert.False(t, DialRefused(ctx, "[2001:db8::1]:443"))
	assert.True(t, DialRefused(ctx, "203.0.113.5:443"))
	assert.True(t, DialRefused(ctx, "example.org:443"))
	assert.False(t, DialRefused(ctx, "192.168.1.20:4242"), "the local network")
	assert.False(t, DialRefused(WithDirect(ctx), "203.0.113.5:443"), "a dial the user marked direct")

	SetDialOnly([]string{})
	assert.True(t, DialRefused(ctx, "198.51.100.7:443"), "an empty list: nowhere")
	_, err := dialTCPOut(ctx, "198.51.100.7:443")
	assert.ErrorIs(t, err, ErrDialRefused, "the TCP dial asks it")
}
