package exit_gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// fakeDoH is a DNS-over-HTTPS server: names in addrs resolve (IPv4 only), others do not exist.
type fakeDoH struct {
	srv      *httptest.Server
	queries  atomic.Int32
	fail     atomic.Bool // answer HTTP 500
	addrs    map[string]net.IP
	lastSize atomic.Int32
}

func newFakeDoH(t *testing.T, addrs map[string]net.IP) *fakeDoH {
	f := &fakeDoH{addrs: addrs}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.queries.Add(1)
		if f.fail.Load() || r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.lastSize.Store(int32(len(body)))
		var p dnsmessage.Parser
		if _, err := p.Start(body); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		q, err := p.Question()
		if err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		ip, known := f.addrs[q.Name.String()]
		h := dnsmessage.Header{Response: true, RecursionAvailable: true}
		if !known {
			h.RCode = dnsmessage.RCodeNameError
		}
		b := dnsmessage.NewBuilder(nil, h)
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		if known && q.Type == dnsmessage.TypeA {
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 300},
				dnsmessage.AResource{A: [4]byte(ip.To4())})
		}
		out, _ := b.Finish()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func testDoH(t *testing.T, fallback func(context.Context, string) ([]net.IP, error), servers ...*fakeDoH) *dohResolver {
	var urls []string
	for _, s := range servers {
		urls = append(urls, s.srv.URL+"/dns-query")
	}
	r := newDoHResolver(urls, fallback)
	r.client = servers[0].srv.Client() // trusts the test certificates
	for _, s := range servers[1:] {
		r.client.Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(s.srv.Certificate())
	}
	return r
}

// The exit resolves its clients' names over HTTPS, not with the machine's resolver in the clear:
// answers are cached, a failing server hands over to the next, and the machine's resolver is asked
// only when every server fails (and only if the operator allows it).
func TestExitResolvesOverDoH(t *testing.T) {
	ctx := context.Background()
	want := net.ParseIP("192.0.2.10").To4()
	first := newFakeDoH(t, map[string]net.IP{"site.example.": want})
	second := newFakeDoH(t, map[string]net.IP{"site.example.": want})
	var system atomic.Int32
	fallback := func(context.Context, string) ([]net.IP, error) {
		system.Add(1)
		return []net.IP{net.ParseIP("198.51.100.1")}, nil
	}
	r := testDoH(t, fallback, first, second)

	ips, err := r.LookupIP(ctx, "Site.Example.")
	require.NoError(t, err)
	assert.Equal(t, []net.IP{want}, ips)
	assert.Equal(t, int32(2), first.queries.Load(), "A and AAAA")
	assert.Zero(t, first.lastSize.Load()%dohPadBlock, "the query is padded to a block")

	_, err = r.LookupIP(ctx, "site.example")
	require.NoError(t, err)
	assert.Equal(t, int32(2), first.queries.Load(), "the second lookup is answered from the cache")

	_, err = r.LookupIP(ctx, "nowhere.example")
	assert.ErrorIs(t, err, errNoSuchHost, "a name that does not exist is an answer, not a failure")
	assert.Zero(t, second.queries.Load())
	assert.Zero(t, system.Load())

	first.fail.Store(true)
	ips, err = r.LookupIP(ctx, "other.example")
	assert.ErrorIs(t, err, errNoSuchHost)
	assert.Positive(t, second.queries.Load(), "the next server answers when the first fails")
	assert.Nil(t, ips)

	second.fail.Store(true)
	ips, err = r.LookupIP(ctx, "third.example")
	require.NoError(t, err)
	assert.Equal(t, int32(1), system.Load(), "every server failed: the machine's resolver")
	assert.Equal(t, "198.51.100.1", ips[0].String())

	strict := testDoH(t, nil, first)
	_, err = strict.LookupIP(ctx, "fourth.example")
	assert.Error(t, err, "no fallback: the lookup fails instead of going out in the clear")
	assert.Equal(t, int32(1), system.Load())
}

func TestExitDNSConfig(t *testing.T) {
	e := NewExitService()
	require.NoError(t, e.SetDNS(nil, true))
	require.NoError(t, e.SetDNS([]string{ExitDNSSystem}, true))
	require.NoError(t, e.SetDNS([]string{"https://dns.example/dns-query"}, false))
	assert.Error(t, e.SetDNS([]string{"8.8.8.8"}, true), "only DNS-over-HTTPS URLs")
	assert.Equal(t, "https://1.1.1.1/dns-query", DefaultExitDNS[0], "a server by address: no lookup in the clear to find it")
}

// A tunnel's target name goes through the configured resolver.
func TestTunnelTargetUsesTheExitResolver(t *testing.T) {
	addr := echoServer(t)
	_, port, _ := net.SplitHostPort(addr)
	doh := newFakeDoH(t, map[string]net.IP{"echo.example.": net.ParseIP("127.0.0.1")})
	e := NewExitService()
	e.allowPrivate.Store(true)
	e.resolver = testDoH(t, nil, doh).LookupIP
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, code := e.connect(ctx, net.JoinHostPort("echo.example", port))
	require.Equal(t, resultOK, code)
	conn.Close()
	assert.Positive(t, doh.queries.Load())
}
