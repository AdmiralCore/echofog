package fakedns

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	require.NoError(t, b.StartQuestions())
	require.NoError(t, b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}))
	out, err := b.Finish()
	require.NoError(t, err)
	return out
}

func TestMappingIsStableAndReversible(t *testing.T) {
	m := New()
	a := m.IPFor("Example.COM.")
	b := m.IPFor("example.com")
	assert.Equal(t, a, b)
	assert.True(t, IsFake(a))
	assert.False(t, a.Equal(TunAddr) || a.Equal(DNSAddr))

	name, ok := m.Domain(a)
	require.True(t, ok)
	assert.Equal(t, "example.com", name)

	other := m.IPFor("telegram.org")
	assert.False(t, a.Equal(other))

	_, ok = m.Domain(net.IPv4(8, 8, 8, 8))
	assert.False(t, ok)
}

func TestTranslateHostPort(t *testing.T) {
	m := New()
	ip := m.IPFor("youtube.com")
	got, ok := m.TranslateHostPort(net.JoinHostPort(ip.String(), "443"))
	assert.True(t, ok)
	assert.Equal(t, "youtube.com:443", got)

	got, ok = m.TranslateHostPort("1.1.1.1:53")
	assert.True(t, ok)
	assert.Equal(t, "1.1.1.1:53", got)

	_, ok = m.TranslateHostPort("198.19.200.200:80") // fake but never handed out
	assert.False(t, ok)
}

func TestAnswerA(t *testing.T) {
	m := New()
	resp, ok := m.Answer(query(t, "discord.com.", dnsmessage.TypeA))
	require.True(t, ok)

	var msg dnsmessage.Message
	require.NoError(t, msg.Unpack(resp))
	assert.Equal(t, uint16(0x1234), msg.Header.ID)
	assert.True(t, msg.Header.Response)
	require.Len(t, msg.Answers, 1)
	a := msg.Answers[0].Body.(*dnsmessage.AResource)
	name, ok := m.Domain(net.IP(a.A[:]))
	require.True(t, ok)
	assert.Equal(t, "discord.com", name)
}

func TestAnswerAAAAIsEmpty(t *testing.T) {
	m := New()
	resp, ok := m.Answer(query(t, "discord.com.", dnsmessage.TypeAAAA))
	require.True(t, ok)
	var msg dnsmessage.Message
	require.NoError(t, msg.Unpack(resp))
	assert.Equal(t, dnsmessage.RCodeSuccess, msg.Header.RCode)
	assert.Empty(t, msg.Answers)
}

func TestOtherQueriesAreForwarded(t *testing.T) {
	m := New()
	for _, q := range [][]byte{
		query(t, "example.com.", dnsmessage.TypeTXT),
		query(t, "example.com.", dnsmessage.TypeMX),
		query(t, "printer.local.", dnsmessage.TypeA),
		query(t, "wpad.", dnsmessage.TypeA),
		[]byte("garbage"),
	} {
		_, ok := m.Answer(q)
		assert.False(t, ok)
	}
}

func TestPoolWrapsAndRecyclesOldest(t *testing.T) {
	m := New()
	first := m.IPFor("first.example")
	m.mu.Lock()
	m.next = poolSize - 2
	m.mu.Unlock()
	m.IPFor("last.example")
	recycled := m.IPFor("new.example") // wrapped onto the first slot
	assert.True(t, recycled.Equal(first))
	name, _ := m.Domain(first)
	assert.Equal(t, "new.example", name)
	assert.NotEqual(t, first, m.IPFor("first.example"))
}
