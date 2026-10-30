package names

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func identity(t *testing.T) (types.IdentityPublicKey, func([]byte) ([]byte, error)) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	var id types.IdentityPublicKey
	copy(id[:], pub)
	return id, func(m []byte) ([]byte, error) { return ed25519.Sign(priv, m), nil }
}

func tonAddr(b byte) *address.Address {
	return address.NewAddress(0, 0, append(make([]byte, 31), b))
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]Network{
		"Alice.TON":         TON,
		" alice.ton. ":      TON,
		"chat.alice-1.ton":  TON,
		"durov.t.me":        Telegram,
		"some_user.t.me":    Telegram,
		"alice.sol":         Solana,
		"sub.alice_x.sol":   Solana,
		"abcd.ton":          TON,
		"a-b-c-d-e-f.ton":   TON,
		"x.y.z.lengthy.ton": TON,
	} {
		n, net, err := Normalize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, net, in)
		assert.Equal(t, strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in)), "."), n)
	}
	for _, in := range []string{
		"", "alice", "abc.ton", "-abcd.ton", "abcd-.ton", "al_ice.ton", "аlice.ton", // Cyrillic "а"
		"bad-name.t.me", "abc.t.me", "a..b.ton", ".ton", "alice.eth", "alice.ton/x",
		strings.Repeat("a", 127) + ".ton",
	} {
		assert.False(t, IsName(in), in)
	}
}

func TestBindingRoundTrip(t *testing.T) {
	id, sign := identity(t)
	owner := TONOwner(tonAddr(7))
	issued := time.Unix(1_790_000_000, 0)
	b, err := NewBinding("Alice.ton", owner, id, issued, sign)
	require.NoError(t, err)

	text := b.Text()
	assert.True(t, strings.HasPrefix(text, RecordPrefix))
	assert.Less(t, len(text), 200, "fits two TON cells")

	got, err := ParseBinding(text)
	require.NoError(t, err)
	assert.Equal(t, id, got.Identity)
	assert.Equal(t, owner, got.Owner)
	assert.True(t, issued.Equal(got.Issued))
	require.NoError(t, got.Verify("alice.ton"))

	// signed for one name: copying the record into another name does not make it point there
	assert.ErrorIs(t, got.Verify("alice2.ton"), ErrBadSignature)
	// a different owner in the record breaks the signature
	got.Owner[len(got.Owner)-1] ^= 1
	assert.ErrorIs(t, got.Verify("alice.ton"), ErrBadSignature)
}

func TestBindingRejectsOtherKey(t *testing.T) {
	id, _ := identity(t)
	_, otherSign := identity(t)
	_, err := NewBinding("alice.ton", TONOwner(tonAddr(1)), id, time.Now(), otherSign)
	assert.ErrorIs(t, err, ErrBadSignature)
}

func TestParseBindingBroken(t *testing.T) {
	id, sign := identity(t)
	b, err := NewBinding("alice.ton", TONOwner(tonAddr(1)), id, time.Now(), sign)
	require.NoError(t, err)
	text := b.Text()
	for _, bad := range []string{"", "hello", RecordPrefix, RecordPrefix + "!!!", text[:len(text)-4], text + "AAAA"} {
		_, err := ParseBinding(bad)
		assert.Error(t, err, bad)
	}
	_, err = ParseBinding("fogcontact1:abc")
	assert.ErrorIs(t, err, ErrNotBinding)
}

func TestTONTextRecord(t *testing.T) {
	for _, n := range []int{1, firstChunkMax, firstChunkMax + 1, firstChunkMax + chunkMax + 5, maxTONText} {
		text := strings.Repeat("x", n-1) + "z"
		rec, err := TONTextRecord(text)
		require.NoError(t, err, n)
		// through a BOC, as it comes from a liteserver
		back, err := cell.FromBOC(rec.ToBOC())
		require.NoError(t, err)
		got, err := ParseTONText(back)
		require.NoError(t, err, n)
		assert.Equal(t, text, got, n)
	}
	_, err := TONTextRecord("")
	assert.Error(t, err)
	_, err = TONTextRecord(strings.Repeat("x", maxTONText+1))
	assert.Error(t, err)

	wallet := cell.BeginCell().MustStoreUInt(0x9fd3, 16).MustStoreAddr(tonAddr(1)).MustStoreUInt(0, 8).EndCell()
	_, err = ParseTONText(wallet)
	assert.Error(t, err, "a wallet record is not text")

	// a chunk count larger than the chunks there are
	short := cell.BeginCell().MustStoreUInt(dnsTextTag, 16).MustStoreUInt(2, 8).MustStoreUInt(1, 8).MustStoreSlice([]byte("a"), 8).EndCell()
	_, err = ParseTONText(short)
	assert.Error(t, err)
}

type fakeTON struct {
	entries map[string]TONEntry
	err     error
	calls   int
}

func (f *fakeTON) LookupTON(_ context.Context, name string) (TONEntry, error) {
	f.calls++
	if f.err != nil {
		return TONEntry{}, f.err
	}
	return f.entries[name], nil
}

func recordValue(t *testing.T, text string) *cell.Cell {
	t.Helper()
	v, err := TONRecordValue(text)
	require.NoError(t, err)
	return v
}

func TestResolve(t *testing.T) {
	id, sign := identity(t)
	_, otherSign := identity(t)
	alice, mallory := tonAddr(1), tonAddr(2)
	good, err := NewBinding("alice.ton", TONOwner(alice), id, time.Now(), sign)
	require.NoError(t, err)
	forged := good
	forged.Sig, _ = otherSign(signedBytes("alice.ton", good.body()))
	copied, err := NewBinding("alice.ton", TONOwner(alice), id, time.Now(), sign)
	require.NoError(t, err)
	signed := func(name string) *cell.Cell {
		b, err := NewBinding(name, TONOwner(alice), id, time.Now(), sign)
		require.NoError(t, err)
		return recordValue(t, b.Text())
	}

	src := &fakeTON{entries: map[string]TONEntry{
		"alice.ton":    {Exists: true, Owner: alice, Record: recordValue(t, good.Text())},
		"sold.ton":     {Exists: true, Owner: mallory, Record: signed("sold.ton")},
		"bare.ton":     {Exists: true, Owner: alice},
		"forged.ton":   {Exists: true, Owner: alice, Record: recordValue(t, forged.Text())},
		"copy.ton":     {Exists: true, Owner: alice, Record: recordValue(t, copied.Text())},
		"junk.ton":     {Exists: true, Owner: alice, Record: recordValue(t, "hello")},
		"broken.t.me":  {Exists: true, Broken: "contract exit code: 7"},
		"auction.ton":  {Exists: true, Record: signed("auction.ton")},
		"alice_x.t.me": {Exists: true, Owner: alice, Record: signed("alice_x.t.me")},
	}}
	r := NewResolver(src)
	ctx := context.Background()

	cases := map[string]Status{
		"Alice.ton":    Verified,
		"alice_x.t.me": Verified,
		"sold.ton":     Stale,    // the record outlived the sale
		"auction.ton":  Stale,    // nobody holds it now
		"bare.ton":     NoRecord, // a name without an EchoFog record
		"nobody.ton":   NotFound,
		"forged.ton":   Invalid, // signed by someone else
		"copy.ton":     Invalid, // alice's record, but signed for alice.ton
		"junk.ton":     Invalid,
		"broken.t.me":  Invalid, // the name's contract threw
	}
	for name, want := range cases {
		res, err := r.Resolve(ctx, name)
		require.NoError(t, err, name)
		assert.Equal(t, want, res.Status, name)
		if want == Verified || want == Stale {
			assert.Equal(t, id, res.Identity, name)
			assert.Equal(t, PeerID(id), res.PeerID(), name)
		}
		if want == Invalid {
			assert.NotEmpty(t, res.Problem, name)
		}
	}
	res, _ := r.Resolve(ctx, "alice.ton")
	assert.Equal(t, alice.String(), res.Owner)

	_, err = r.Resolve(ctx, "alice.sol")
	assert.ErrorIs(t, err, ErrUnsupported)
	_, err = r.Resolve(ctx, "alice")
	assert.ErrorIs(t, err, ErrNotName)
}

func TestResolveCache(t *testing.T) {
	id, sign := identity(t)
	alice := tonAddr(1)
	b, err := NewBinding("alice.ton", TONOwner(alice), id, time.Now(), sign)
	require.NoError(t, err)
	src := &fakeTON{entries: map[string]TONEntry{
		"alice.ton": {Exists: true, Owner: alice, Record: recordValue(t, b.Text())},
	}}
	now := time.Unix(1_790_000_000, 0)
	r := NewResolver(src)
	r.Now = func() time.Time { return now }
	ctx := context.Background()

	_, err = r.Resolve(ctx, "alice.ton")
	require.NoError(t, err)
	_, err = r.Resolve(ctx, "ALICE.ton")
	require.NoError(t, err)
	assert.Equal(t, 1, src.calls, "the second lookup is served from the cache")

	_, err = r.Resolve(ctx, "nobody.ton")
	require.NoError(t, err)
	now = now.Add(r.NegativeTTL + time.Second)
	_, err = r.Resolve(ctx, "nobody.ton")
	require.NoError(t, err)
	assert.Equal(t, 3, src.calls, "a missing name is asked again sooner")

	// the chain cannot be read: the last answer is given, marked as such
	now = now.Add(r.TTL)
	src.err = errors.New("liteservers unreachable")
	res, err := r.Resolve(ctx, "alice.ton")
	require.NoError(t, err)
	assert.True(t, res.Cached)
	assert.Equal(t, Verified, res.Status)

	// ... but not forever
	now = now.Add(r.MaxStale)
	_, err = r.Resolve(ctx, "alice.ton")
	assert.Error(t, err)

	// a name never seen while offline is an error, not "not found"
	_, err = r.Resolve(ctx, "other.ton")
	assert.Error(t, err)

	src.err = nil
	r.Forget("alice.ton")
	res, err = r.Resolve(ctx, "alice.ton")
	require.NoError(t, err)
	assert.False(t, res.Cached)
}
