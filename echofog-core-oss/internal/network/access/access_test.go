package access

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func peer(b byte) types.PeerID {
	var id types.PeerID
	id[0] = b
	return id
}

func TestOpenGateLetsEverybodyIn(t *testing.T) {
	g := NewGate()
	assert.True(t, g.Allowed(peer(1)))
	assert.True(t, g.AdmitTicket(peer(1), nil))
}

func TestTicket(t *testing.T) {
	g := NewGate()
	secret := NewSecret()
	require.NoError(t, g.SetSecret(secret))
	g.Enable(true)
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }

	assert.False(t, g.AdmitTicket(peer(1), nil), "no ticket")
	assert.False(t, g.AdmitTicket(peer(1), Ticket(NewSecret(), peer(1), now)), "another node's secret")
	assert.False(t, g.AdmitTicket(peer(2), Ticket(secret, peer(1), now)), "a ticket made for somebody else")
	assert.False(t, g.AdmitTicket(peer(1), Ticket(secret, peer(1), now.Add(-10*time.Minute))), "an old ticket")

	var joined types.PeerID
	g.OnJoin(func(id types.PeerID) { joined = id })
	assert.True(t, g.AdmitTicket(peer(1), Ticket(secret, peer(1), now.Add(-90*time.Second))), "a minute of clock skew")
	assert.Equal(t, peer(1), joined)
	assert.True(t, g.Allowed(peer(1)))
	assert.False(t, g.Allowed(peer(2)))
}

func TestGateWithoutSecretLetsNobodyNewIn(t *testing.T) {
	g := NewGate()
	g.Enable(true)
	assert.False(t, g.AdmitTicket(peer(1), Ticket(NewSecret(), peer(1), time.Now())))
	assert.ErrorIs(t, g.SetSecret([]byte("short")), ErrBadSecret)
}

func TestLinkRoundTrip(t *testing.T) {
	inv := Invite{
		Network:  "echofog",
		Protocol: 101,
		Peer:     peer(7),
		Secret:   NewSecret(),
		Addrs:    []string{"203.0.113.5:4242", "wss://cdn.example.org/echofog"},
		Relays:   []string{"198.51.100.1:443/" + peer(9).String()},
	}
	link := FormatLink(inv)
	assert.True(t, strings.HasPrefix(link, "echofog://join?v=1&net=echofog&p=101&id="), link)
	assert.True(t, IsLink("  "+strings.ToUpper(link[:15])+link[15:]))

	got, err := ParseLink(link, "echofog")
	require.NoError(t, err)
	assert.Equal(t, LinkVersion, got.Version)
	assert.Equal(t, inv.Peer, got.Peer)
	assert.Equal(t, inv.Secret, got.Secret)
	assert.Equal(t, inv.Addrs, got.Addrs)
	assert.Equal(t, inv.Relays, got.Relays)
	assert.Equal(t, uint32(101), got.Protocol)

	inv.Secret = nil
	got, err = ParseLink(FormatLink(inv), "echofog")
	require.NoError(t, err)
	assert.Nil(t, got.Secret, "a public node's link carries no secret")
}

func TestLinkOfAnotherNetworkOrVersion(t *testing.T) {
	link := FormatLink(Invite{Network: "myfork", Peer: peer(1), Addrs: []string{"203.0.113.5:4242"}})
	_, err := ParseLink(link, "echofog")
	var other *OtherNetworkError
	require.True(t, errors.As(err, &other))
	assert.Equal(t, "myfork", other.Network)

	newer := strings.Replace(link, "v=1", "v=2", 1)
	_, err = ParseLink(strings.Replace(newer, "net=myfork", "net=echofog", 1), "echofog")
	assert.ErrorIs(t, err, ErrLinkTooNew)

	_, err = ParseLink("echofog://relay/1.2.3.4:5/abc", "echofog")
	assert.ErrorIs(t, err, ErrNotInvite)
}

func TestLinkRejectsBrokenOnes(t *testing.T) {
	good := FormatLink(Invite{Network: "echofog", Peer: peer(1), Secret: NewSecret(), Addrs: []string{"203.0.113.5:4242"}})
	for name, link := range map[string]string{
		"no version":  strings.Replace(good, "v=1&", "", 1),
		"bad id":      strings.Replace(good, "id="+peer(1).String(), "id=zz", 1),
		"bad secret":  good[:strings.Index(good, "&s=")] + "&s=abc" + good[strings.Index(good, "&a="):],
		"no address":  good[:strings.Index(good, "&a=")],
		"no protocol": strings.Replace(good, "v=1&net=echofog", "v=1&net=echofog&p=x", 1),
	} {
		_, err := ParseLink(link, "echofog")
		assert.Error(t, err, name)
	}
}

func TestKeyring(t *testing.T) {
	k := NewKeyring()
	assert.Nil(t, k.Ticket(peer(1), peer(2)))
	s := NewSecret()
	require.NoError(t, k.Set(peer(1), s))
	assert.Equal(t, s, k.Get(peer(1)))
	assert.Len(t, k.Ticket(peer(1), peer(2)), ticketLen)
	require.NoError(t, k.Set(peer(1), nil))
	assert.Nil(t, k.Get(peer(1)))
	assert.ErrorIs(t, k.Set(peer(1), []byte{1}), ErrBadSecret)
}

// The link of a server carries its cover site's name and its role, so whoever follows it dials the
// node as REALITY, and never as noise if a later path loses the SNI.
func TestLinkCarriesSNIAndServerRole(t *testing.T) {
	inv := Invite{Network: "echofog", Peer: peer(9), Addrs: []string{"203.0.113.5:4242"}, SNI: "cover.example", Server: true,
		Relays: []string{"203.0.113.6:4242/" + peer(8).String() + "?sni=cover.example&srv=1"}}
	got, err := ParseLink(FormatLink(inv), "echofog")
	require.NoError(t, err)
	assert.Equal(t, "cover.example", got.SNI)
	assert.True(t, got.Server)
	assert.Equal(t, inv.Relays, got.Relays, "the relay's own SNI and role survive the link")

	plain, err := ParseLink(FormatLink(Invite{Network: "echofog", Peer: peer(9), Addrs: []string{"203.0.113.5:4242"}}), "echofog")
	require.NoError(t, err)
	assert.False(t, plain.Server)
	assert.Empty(t, plain.SNI)
}
