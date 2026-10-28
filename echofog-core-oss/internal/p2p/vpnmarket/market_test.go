package vpnmarket

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemStore() *memStore { return &memStore{m: map[string][]byte{}} }

func (s *memStore) Set(k, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(k)] = append([]byte(nil), v...)
	return nil
}
func (s *memStore) Get(k []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[string(k)]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}
func (s *memStore) Delete(k []byte) error { delete(s.m, string(k)); return nil }
func (s *memStore) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Entry
	for k, v := range s.m {
		if strings.HasPrefix(k, string(prefix)) {
			out = append(out, storage.Entry{Key: []byte(k), Value: v})
		}
	}
	return out, nil
}
func (s *memStore) FindValuesSeek(prefix, start []byte, limit int) ([]storage.Entry, error) {
	return s.FindValues(prefix, limit)
}
func (s *memStore) Exists(k []byte) bool { _, ok := s.m[string(k)]; return ok }
func (s *memStore) Close()               {}

type fakeDHT struct {
	mu      sync.Mutex
	data    map[string][]byte
	peers   []types.PeerID
	failSet bool
}

func newFakeDHT() *fakeDHT { return &fakeDHT{data: map[string][]byte{}} }

func (f *fakeDHT) Set(k, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet {
		return errors.New("no peers to store on")
	}
	f.data[string(k)] = append([]byte(nil), v...)
	return nil
}
func (f *fakeDHT) GetFresh(k []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.data[string(k)]; ok {
		return v, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeDHT) KnownPeers() []types.PeerID { return f.peers }

type identity struct {
	priv ed25519.PrivateKey
	pub  types.PeerPublicKey
	id   types.PeerID
}

func (i identity) CreateSignature(data []byte) ([]byte, error) {
	return ed25519.Sign(i.priv, data), nil
}

func newIdentity(t *testing.T) identity {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	return identity{priv: priv, pub: pk, id: types.PeerPubKeyToID(pk)}
}

func publisherFor(d DHTClient, i identity, neighbors ...string) *Publisher {
	return NewPublisher(d, i, i.id, i.pub, func() []string { return neighbors })
}

func signedAdvert(t *testing.T, i identity, mutate func(a *Advert)) []byte {
	now := time.Now()
	a := &Advert{Version: 1, PeerID: i.id.String(), PubKey: i.pub[:], Enabled: true, PricePerMB: 10, UploadKbps: 50000,
		FreeTestKB: 512, IssuedAt: now.Unix(), ExpiresAt: now.Add(AdvertTTL).Unix()}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, a.Sign(i))
	raw, err := a.Encode()
	require.NoError(t, err)
	return raw
}

func TestAdvertVerification(t *testing.T) {
	i := newIdentity(t)
	now := time.Now()

	a, err := Decode(signedAdvert(t, i, nil), now)
	require.NoError(t, err)
	assert.Equal(t, int64(10), a.PricePerMB)

	cases := map[string]func(a *Advert){
		"future":       func(a *Advert) { a.IssuedAt = now.Add(time.Hour).Unix(); a.ExpiresAt = now.Add(2 * time.Hour).Unix() },
		"too long ttl": func(a *Advert) { a.ExpiresAt = now.Add(3 * time.Hour).Unix() },
		"negative":     func(a *Advert) { a.PricePerMB = -1 },
		"free too big": func(a *Advert) { a.FreeTestKB = 5000 },
		"bad neighbor": func(a *Advert) { a.Neighbors = []string{"zz"} },
	}
	for name, mutate := range cases {
		_, err := Decode(signedAdvert(t, i, mutate), now)
		assert.Error(t, err, name)
	}

	_, err = Decode(signedAdvert(t, i, func(a *Advert) {
		a.IssuedAt = now.Add(-time.Hour).Unix()
		a.ExpiresAt = now.Add(-30 * time.Minute).Unix()
	}), now)
	assert.ErrorIs(t, err, ErrAdvertExpired)

	other := newIdentity(t)
	_, err = Decode(signedAdvert(t, i, func(a *Advert) { a.PeerID = other.id.String() }), now)
	assert.Error(t, err, "peer id must match the public key")

	tampered := signedAdvert(t, i, nil)
	tampered = []byte(strings.Replace(string(tampered), `"price_per_mb":10`, `"price_per_mb":1`, 1))
	_, err = Decode(tampered, now)
	assert.Error(t, err, "changed field must break the signature")

	_, err = Decode([]byte("not json"), now)
	assert.Error(t, err)
}

func TestPublisherPublishesRepublishesAndWithdraws(t *testing.T) {
	d := newFakeDHT()
	i := newIdentity(t)
	p := publisherFor(d, i)
	p.interval = 20 * time.Millisecond

	require.NoError(t, p.Start(context.Background(), ShareConfig{PricePerMB: 7, UploadKbps: 20000, FreeTestKB: 256}))
	raw, err := d.GetFresh(Key(i.id))
	require.NoError(t, err)
	a, err := Decode(raw, time.Now())
	require.NoError(t, err)
	assert.True(t, a.Enabled)
	assert.Equal(t, 256, a.FreeTestKB)

	first := p.Status().LastPublish
	require.Eventually(t, func() bool { return p.Status().LastPublish >= first && p.Status().Running }, time.Second, 10*time.Millisecond)
	time.Sleep(1100 * time.Millisecond)
	assert.Greater(t, p.Status().LastPublish, first, "advert must be refreshed automatically")

	require.NoError(t, p.Update(ShareConfig{PricePerMB: 9, UploadKbps: 20000, FreeTestKB: 256}))
	raw, _ = d.GetFresh(Key(i.id))
	a, _ = Decode(raw, time.Now())
	assert.Equal(t, int64(9), a.PricePerMB)

	require.NoError(t, p.Stop())
	raw, _ = d.GetFresh(Key(i.id))
	a, err = Decode(raw, time.Now())
	require.NoError(t, err)
	assert.False(t, a.Enabled, "stopping must publish a signed withdrawal")
	assert.False(t, p.Status().Running)

	assert.Error(t, p.Start(context.Background(), ShareConfig{FreeTestKB: 5000}))
}

func TestDirectoryDiscoveryNeighborsSpoofAndWithdrawal(t *testing.T) {
	d := newFakeDHT()
	a, b, c, me := newIdentity(t), newIdentity(t), newIdentity(t), newIdentity(t)

	require.NoError(t, publisherFor(d, a, b.id.String()).Start(context.Background(), ShareConfig{PricePerMB: 5, UploadKbps: 10000}))
	require.NoError(t, publisherFor(d, b, c.id.String()).Start(context.Background(), ShareConfig{PricePerMB: 5, UploadKbps: 10000}))
	pc := publisherFor(d, c)
	require.NoError(t, pc.Start(context.Background(), ShareConfig{PricePerMB: 1, UploadKbps: 30000}))

	// only a is known up front; b is found through a, c through b
	d.peers = []types.PeerID{a.id}
	dir := NewDirectory(d, NewRatingStore(newMemStore()), me.id, nil)
	assert.Equal(t, 3, dir.Refresh(context.Background()))

	// a spoofed advert placed under another peer's key is ignored
	spoofKey := Key(newIdentity(t).id)
	victim := newIdentity(t)
	require.NoError(t, d.Set(spoofKey, signedAdvert(t, victim, nil)))
	d.peers = append(d.peers, victim.id)
	for _, e := range dir.List() {
		assert.NotEqual(t, victim.id.String(), e.Advert.PeerID)
	}

	require.NoError(t, pc.Stop())
	assert.Equal(t, 2, dir.Refresh(context.Background()))

	for _, e := range dir.List() {
		assert.NotEqual(t, me.id.String(), e.Advert.PeerID)
	}
}

func TestRatingsAndScoring(t *testing.T) {
	store := NewRatingStore(newMemStore())
	var good, liar, flaky types.PeerID
	good[0], liar[0], flaky[0] = 1, 2, 3
	now := time.Now()

	adv := &Advert{PricePerMB: 10, UploadKbps: 50000}
	for _, p := range []types.PeerID{good, liar, flaky} {
		require.NoError(t, store.RecordAdvert(p, adv, now))
	}
	for i := 0; i < 5; i++ {
		require.NoError(t, store.RecordProbe(good, ProbeResult{At: now, OK: true, LatencyMs: 80, MeasuredKbps: 48000, DomainsTried: 3, DomainsOK: 3}))
		require.NoError(t, store.RecordProbe(liar, ProbeResult{At: now, OK: true, LatencyMs: 80, MeasuredKbps: 2000, DomainsTried: 3, DomainsOK: 3}))
		require.NoError(t, store.RecordProbe(flaky, ProbeResult{At: now, OK: false, DomainsTried: 3}))
	}

	g, _ := store.Get(good)
	l, _ := store.Get(liar)
	f, _ := store.Get(flaky)
	assert.Equal(t, 5, g.Probes)
	assert.Equal(t, 5, f.ConsecutiveFails)
	assert.Greater(t, Score(g, adv), Score(l, adv), "an exit that over-claims bandwidth ranks lower")
	assert.Greater(t, Score(l, adv), Score(f, adv), "an unreachable exit ranks lowest")

	cheap := &Advert{PricePerMB: 1}
	dear := &Advert{PricePerMB: 5000}
	assert.Greater(t, Score(g, cheap), Score(g, dear))

	assert.Len(t, store.All(), 3)

	reopened := NewRatingStore(store.st)
	g2, ok := reopened.Get(good)
	require.True(t, ok, "ratings must survive in the database")
	assert.Equal(t, g.MeasuredKbps, g2.MeasuredKbps)
}

type fakeTunnel struct {
	results map[string]error
	delay   time.Duration
}

func (f fakeTunnel) TestDial(_ context.Context, _ types.PeerID, target string) (time.Duration, error) {
	if err, ok := f.results[target]; ok && err != nil {
		return 0, err
	}
	return f.delay, nil
}

type fakeBW struct{ kbps int }

func (f fakeBW) MeasureDownload(context.Context, types.PeerID) (int, error) { return f.kbps, nil }

type okConnector struct{ err error }

func (o okConnector) Ensure(context.Context, types.PeerID) error { return o.err }

func newService(t *testing.T, tun Tunneler, bw Bandwidth, conn Connector) (*Service, *RatingStore) {
	st := newMemStore()
	rs := NewRatingStore(st)
	me := newIdentity(t)
	d := newFakeDHT()
	return NewService(publisherFor(d, me), NewDirectory(d, rs, me.id, nil), rs, st, tun, bw, conn), rs
}

func TestProbeRecordsAvailabilityAndSpeed(t *testing.T) {
	svc, rs := newService(t, fakeTunnel{
		delay:   40 * time.Millisecond,
		results: map[string]error{"www.google.com:443": errors.New("blocked")},
	}, fakeBW{kbps: 12000}, okConnector{})

	var peer types.PeerID
	peer[0] = 5
	res, err := svc.Probe(context.Background(), peer)
	require.NoError(t, err)
	assert.True(t, res.OK)
	assert.Equal(t, 3, res.DomainsTried)
	assert.Equal(t, 2, res.DomainsOK)

	r, ok := rs.Get(peer)
	require.True(t, ok)
	assert.Equal(t, 12000, r.MeasuredKbps)
	assert.Equal(t, 2, r.DomainsOK)
	assert.InDelta(t, 40, r.LatencyMs, 1)
}

func TestProbeTreatsPaymentRequiredAsReachableAndFailuresAsFailures(t *testing.T) {
	svc, rs := newService(t, fakeTunnel{results: map[string]error{
		"www.cloudflare.com:443": exit_gateway.ErrPaymentRequired,
	}}, nil, okConnector{})
	var peer types.PeerID
	peer[0] = 6
	res, err := svc.Probe(context.Background(), peer)
	require.NoError(t, err)
	assert.True(t, res.OK)
	assert.True(t, res.PaymentRequired)
	r, _ := rs.Get(peer)
	assert.Equal(t, 0, r.ConsecutiveFails)

	svc2, rs2 := newService(t, fakeTunnel{results: map[string]error{
		"www.cloudflare.com:443": errors.New("x"), "www.wikipedia.org:443": errors.New("x"), "www.google.com:443": errors.New("x"),
	}}, nil, okConnector{})
	res, err = svc2.Probe(context.Background(), peer)
	require.NoError(t, err)
	assert.False(t, res.OK)
	r, _ = rs2.Get(peer)
	assert.Equal(t, 1, r.ConsecutiveFails)

	svc3, rs3 := newService(t, fakeTunnel{}, nil, okConnector{err: errors.New("unreachable")})
	_, err = svc3.Probe(context.Background(), peer)
	assert.Error(t, err)
	r, _ = rs3.Get(peer)
	assert.Equal(t, 1, r.ConsecutiveFails, "a failed connection counts as a failed probe")
}

type countingBW struct {
	kbps  int
	calls int
}

func (c *countingBW) MeasureDownload(context.Context, types.PeerID) (int, error) {
	c.calls++
	return c.kbps, nil
}

// The background check of exits sends no download burst: a dozen MB over the plain mesh link to
// every exit, from every node, is the noise a censor flags. Only a check the user asks for does.
func TestAutoProbeSendsNoDownloadBurst(t *testing.T) {
	bw := &countingBW{kbps: 30000}
	d := newFakeDHT()
	exit, me := newIdentity(t), newIdentity(t)
	require.NoError(t, publisherFor(d, exit).Start(context.Background(), ShareConfig{PricePerMB: 5, UploadKbps: 40000}))
	d.peers = []types.PeerID{exit.id}
	st := newMemStore()
	rs := NewRatingStore(st)
	svc := NewService(publisherFor(d, me), NewDirectory(d, rs, me.id, nil), rs, st, fakeTunnel{delay: 30 * time.Millisecond}, bw, okConnector{})

	assert.Equal(t, 1, svc.SyncOnce(context.Background()))
	r, ok := rs.Get(exit.id)
	require.True(t, ok)
	assert.Equal(t, 1, r.Probes, "the exit was checked")
	assert.Equal(t, 3, r.DomainsOK)
	assert.InDelta(t, 30, r.LatencyMs, 1)
	assert.Zero(t, bw.calls, "no download burst in a background check")
	assert.Zero(t, r.MeasuredKbps)

	_, err := svc.Probe(context.Background(), exit.id)
	require.NoError(t, err)
	assert.Equal(t, 1, bw.calls, "a check asked for by the user measures the speed")
	r, _ = rs.Get(exit.id)
	assert.Equal(t, 30000, r.MeasuredKbps)
}

// What a tunnel really delivered is a lower bound on the exit's speed: it raises the rating, never
// lowers it, and a slow stretch (the user asked for little) never ranks an exit below unmeasured.
func TestThroughputOnlyRaisesTheMeasuredSpeed(t *testing.T) {
	svc, rs := newService(t, fakeTunnel{}, nil, okConnector{})
	var exit, unknown, slowProbed types.PeerID
	exit[0], unknown[0], slowProbed[0] = 1, 2, 3
	adv := &Advert{UploadKbps: 50000}
	now := time.Now()
	require.NoError(t, rs.RecordAdvert(exit, adv, now))
	require.NoError(t, rs.RecordAdvert(slowProbed, adv, now))
	unmeasured, _ := rs.Get(exit)

	require.NoError(t, svc.NoteThroughput(exit, 10000))
	r, _ := rs.Get(exit)
	assert.Zero(t, r.MeasuredKbps, "10 of 50 Mbit/s says how much was asked, not what the exit can do")

	require.NoError(t, svc.NoteThroughput(exit, 40000))
	r, _ = rs.Get(exit)
	assert.Equal(t, 40000, r.MeasuredKbps)
	assert.Greater(t, Score(r, adv), Score(unmeasured, adv), "an exit seen delivering near its claim ranks above an unmeasured one")

	require.NoError(t, svc.NoteThroughput(exit, 20000))
	require.NoError(t, svc.NoteThroughput(exit, 0))
	r, _ = rs.Get(exit)
	assert.Equal(t, 40000, r.MeasuredKbps, "a slower stretch later does not lower it")

	require.NoError(t, svc.NoteThroughput(unknown, 40000))
	_, ok := rs.Get(unknown)
	assert.False(t, ok, "an exit never seen in an advert gets no rating from traffic")

	require.NoError(t, rs.RecordProbe(slowProbed, ProbeResult{At: now, OK: true, MeasuredKbps: 5000}))
	require.NoError(t, svc.NoteThroughput(slowProbed, 8000))
	r, _ = rs.Get(slowProbed)
	assert.Equal(t, 8000, r.MeasuredKbps, "once measured, any faster real rate raises it")
}

func TestDomainsConfig(t *testing.T) {
	svc, _ := newService(t, fakeTunnel{}, nil, okConnector{})
	assert.Equal(t, DefaultProbeDomains, svc.Domains())
	require.NoError(t, svc.SetDomains([]string{"example.org:443"}))
	assert.Equal(t, []string{"example.org:443"}, svc.Domains())
	many := make([]string, maxProbeDomains+1)
	for i := range many {
		many[i] = fmt.Sprintf("site%d.example:443", i)
	}
	assert.Error(t, svc.SetDomains(many), "more than %d sites", maxProbeDomains)
	assert.NoError(t, svc.SetDomains(many[:maxProbeDomains]))
}

func TestDirectoryFindsExitsThroughPeersHints(t *testing.T) {
	d := newFakeDHT() // the DHT knows nothing: the exit sits behind NAT, nobody can look it up
	natExit, neighbour, me := newIdentity(t), newIdentity(t), newIdentity(t)
	require.NoError(t, publisherFor(d, neighbour).Start(context.Background(), ShareConfig{PricePerMB: 5, UploadKbps: 10000}))
	d.data = map[string][]byte{string(Key(neighbour.id)): d.data[string(Key(neighbour.id))]}

	// a relay we are connected to knows the exit, and the exit names a neighbour
	relaySays := [][]byte{
		signedAdvert(t, natExit, func(a *Advert) { a.Neighbors = []string{neighbour.id.String()} }),
		[]byte("not an advert"),
		signedAdvert(t, newIdentity(t), func(a *Advert) { a.PricePerMB = -1 }), // fails verification
	}
	dir := NewDirectory(d, NewRatingStore(newMemStore()), me.id, nil)
	dir.SetHints(func(context.Context) [][]byte { return relaySays })

	assert.Equal(t, 2, dir.Refresh(context.Background()), "the exit from the hint, and its neighbour through the DHT")
	_, ok := dir.Get(natExit.id)
	assert.True(t, ok)

	// what we pass on verifies at the next peer
	shared := dir.Shared()
	require.Len(t, shared, 2)
	for _, raw := range shared {
		_, err := Decode(raw, time.Now())
		assert.NoError(t, err)
	}
}

// A sharing exit hands out its own advert; one that could not start, or stopped, has none.
func TestPublisherCurrentAdvert(t *testing.T) {
	d := newFakeDHT()
	d.failSet = true
	i := newIdentity(t)
	p := publisherFor(d, i)
	assert.Nil(t, p.Current(), "not sharing yet")

	_ = p.Start(context.Background(), ShareConfig{PricePerMB: 7, UploadKbps: 20000, FreeTestKB: 256})
	assert.Nil(t, p.Current(), "a failed start does not share")

	d.failSet = false
	require.NoError(t, p.Start(context.Background(), ShareConfig{PricePerMB: 7, UploadKbps: 20000, FreeTestKB: 256}))
	a, err := Decode(p.Current(), time.Now())
	require.NoError(t, err)
	assert.True(t, a.Enabled)
	id, _ := a.ID()
	assert.Equal(t, i.id, id)

	require.NoError(t, p.Stop())
	assert.Nil(t, p.Current(), "withdrawn")
}

// The Check sites lists live in the node's store: added sites, and whether the user ever chose.
func TestCustomSitesAndChosenFlag(t *testing.T) {
	svc, _ := newService(t, fakeTunnel{}, nil, okConnector{})
	assert.False(t, svc.DomainsSaved(), "nothing chosen yet: the app may offer its starter set")
	assert.Empty(t, svc.CustomSites())

	require.NoError(t, svc.SetCustomSites([]string{"vk.com:443", "ya.ru:443"}))
	assert.Equal(t, []string{"vk.com:443", "ya.ru:443"}, svc.CustomSites())
	require.NoError(t, svc.SetDomains([]string{"vk.com:443"}))
	assert.True(t, svc.DomainsSaved())
	require.NoError(t, svc.SetDomains([]string{}))
	assert.True(t, svc.DomainsSaved(), "an empty choice is still a choice")
	assert.Empty(t, svc.SavedDomains())
	assert.Equal(t, DefaultProbeDomains, svc.Domains(), "probing still has sites to check")

	assert.Error(t, svc.SetCustomSites(make([]string, maxCustomSites+1)))
}

// The camouflage SNI in an exit's advert reaches whoever dials the exit: the directory hands it to
// the node as it takes the advert in, so the first dial to an exit known only from the market is
// REALITY, not noise. A withdrawn advert and the node's own are not passed on.
func TestDirectoryPassesOnTheAdvertisedSNI(t *testing.T) {
	d := newFakeDHT()
	cam, ordinary, me := newIdentity(t), newIdentity(t), newIdentity(t)
	pc := publisherFor(d, cam)
	pc.SetSNI("cover.example")
	require.NoError(t, pc.Start(context.Background(), ShareConfig{PricePerMB: 5, UploadKbps: 10000}))
	require.NoError(t, publisherFor(d, ordinary).Start(context.Background(), ShareConfig{PricePerMB: 5, UploadKbps: 10000}))

	got := map[types.PeerID]string{}
	dir := NewDirectory(d, NewRatingStore(newMemStore()), me.id, nil)
	dir.SetSNIHook(func(id types.PeerID, sni string) { got[id] = sni })
	d.peers = []types.PeerID{cam.id, ordinary.id}
	require.Equal(t, 2, dir.Refresh(context.Background()))
	assert.Equal(t, map[types.PeerID]string{cam.id: "cover.example"}, got, "only the camouflage exit has an SNI to pass on")
}

// A node's relay price rides with its advert, and a free relay adds nothing to it.
func TestRelayPriceInAdvert(t *testing.T) {
	a := &Advert{Version: AdvertVersion, Enabled: true, RelayPricePerMB: 42}
	assert.Equal(t, int64(42), a.RelayPricePerMB)
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"relay_price_per_mb":42`)
	free, _ := json.Marshal(&Advert{Version: AdvertVersion})
	assert.NotContains(t, string(free), "relay_price", "a free relay adds nothing to the advert")
}
