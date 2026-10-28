package main

import (
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
)

// The join address a server prints carries its cover site and its role, whatever the role is, so
// that a client pasting it into --bootstrap dials it as REALITY from the first packet.
func TestJoinAddressCarriesSNIAndRole(t *testing.T) {
	var id types.PeerID
	id[0] = 7

	server := config.DefaultConfig("")
	server.Role = config.RoleServer
	server.AntiDPI.TLSServerName = "cover.example"
	info := network.ParseAddrInfo(composeJoin("203.0.113.15:4242", id, server))
	assert.Equal(t, network.AddrInfo{Addr: "203.0.113.15:4242", ID: id, Named: true, SNI: "cover.example", Server: true}, info)

	bare := config.DefaultConfig("")
	bare.Role = config.RoleServer
	assert.True(t, network.ParseAddrInfo(composeJoin("203.0.113.15:4242", id, bare)).Server, "a server without a cover site still says it is one")

	client := config.DefaultConfig("")
	assert.Equal(t, "203.0.113.15:4242/"+id.String(), composeJoin("203.0.113.15:4242", id, client))
}
