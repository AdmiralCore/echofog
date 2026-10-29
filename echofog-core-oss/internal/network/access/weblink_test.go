package access

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An invitation goes out as an https page a messenger makes tappable and comes back whole: the
// app reads it as the echofog:// link it carries after "#", secret included.
func TestWebLinkRoundTrip(t *testing.T) {
	inv := Invite{Network: "echofog", Protocol: 101, Peer: peer(7), Secret: NewSecret(),
		Addrs: []string{"203.0.113.5:4242"}, SNI: "cover.example", Server: true}
	link := FormatLink(inv)
	web := WebLink(link)
	require.True(t, strings.HasPrefix(web, WebLinkBase+"join?v=1&net=echofog"), web)
	assert.Equal(t, link, FromWebLink(web))
	assert.True(t, IsLink(web))

	got, err := ParseLink(web, "echofog")
	require.NoError(t, err)
	assert.Equal(t, inv.Peer, got.Peer)
	assert.Equal(t, inv.Secret, got.Secret, "the secret rides after # and comes back")
	assert.Equal(t, inv.Addrs, got.Addrs)
}

// Any page whose fragment is an EchoFog link is read (a fork's page, a whole link after #); other
// text, and other kinds of URL, are left as they are.
func TestFromWebLink(t *testing.T) {
	relay := "echofog://relay/198.51.100.1:443/" + peer(9).String() + "?sni=cover.example&srv=1"
	for in, want := range map[string]string{
		"https://admiralcore.github.io/echofog/join/#relay/198.51.100.1:443/" + peer(9).String() + "?sni=cover.example&srv=1": relay,
		"  https://fork.example/join#echofog://join?v=1&net=echofog&id=" + peer(7).String() + " ":                             "echofog://join?v=1&net=echofog&id=" + peer(7).String(),
		"http://localhost:8080/join/#JOIN?v=1": "echofog://JOIN?v=1",
		"https://example.org/page#section":     "https://example.org/page#section",
		"https://example.org/join/":            "https://example.org/join/",
		"javascript:alert(1)#join?v=1":         "javascript:alert(1)#join?v=1",
		"echofog://relay/a:1/b":                "echofog://relay/a:1/b",
		"fogcontact1:abc":                      "fogcontact1:abc",
	} {
		assert.Equal(t, want, FromWebLink(in), in)
	}
	assert.Equal(t, "", WebLink("fogcontact1:abc"), "only echofog:// links have a web form")
	assert.Equal(t, WebLinkBase+"relay/a:1/b", WebLink("ECHOFOG://relay/a:1/b"))
}
