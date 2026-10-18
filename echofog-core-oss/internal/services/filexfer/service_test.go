package filexfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStore is an in-memory storage.Storage.
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
	v, ok := s.m[string(k)]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), v...), nil
}
func (s *memStore) Delete(k []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(k))
	return nil
}
func (s *memStore) FindValues(prefix []byte, limit int) ([]storage.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.m {
		if strings.HasPrefix(k, string(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []storage.Entry
	for _, k := range keys {
		out = append(out, storage.Entry{Key: []byte(k), Value: append([]byte(nil), s.m[k]...)})
	}
	return out, nil
}
func (s *memStore) FindValuesSeek(prefix, _ []byte, limit int) ([]storage.Entry, error) {
	return s.FindValues(prefix, limit)
}
func (s *memStore) Exists(k []byte) bool { _, err := s.Get(k); return err == nil }
func (s *memStore) Close()               {}

// fakeNet carries frames between services, losing, altering or refusing them on request.
type fakeNet struct {
	mu       sync.Mutex
	nodes    map[types.PeerID]*Service
	lossPct  int64                     // frames silently lost, in percent
	mutate   func(frame []byte) []byte // changes a frame on the way (nil: none)
	down     atomic.Bool               // nothing gets through, sends fail
	chunks   atomic.Int64              // chunk frames delivered
	hold     atomic.Bool               // chunk frames are lost (control frames still pass)
	noCancel atomic.Bool               // cancel frames are lost
}

func (n *fakeNet) set(id types.PeerID, s *Service) {
	n.mu.Lock()
	n.nodes[id] = s
	n.mu.Unlock()
}

type fakeLink struct{ net *fakeNet }

func (l fakeLink) Send(_ context.Context, peer types.PeerID, frame []byte) error {
	n := l.net
	if n.down.Load() {
		return errors.New("unreachable")
	}
	n.mu.Lock()
	dst := n.nodes[peer]
	loss, mutate := n.lossPct, n.mutate
	n.mu.Unlock()
	if dst == nil {
		return errors.New("unknown peer")
	}
	if loss > 0 {
		if r, _ := rand.Int(rand.Reader, big.NewInt(100)); r.Int64() < loss {
			return nil
		}
	}
	f := append([]byte(nil), frame...)
	if mutate != nil {
		f = mutate(f)
	}
	if len(f) > 1 && (frameKind(f[1]) == kindChunk && n.hold.Load() || frameKind(f[1]) == kindCancel && n.noCancel.Load()) {
		return nil
	}
	if len(f) > 1 && frameKind(f[1]) == kindChunk {
		n.chunks.Add(1)
	}
	go dst.HandleFrame(f)
	return nil
}

type side struct {
	id    types.PeerID
	store *memStore
	dir   string
	svc   *Service
	mu    sync.Mutex
	seen  []Info
}

func (s *side) events() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Info(nil), s.seen...)
}

func peerID(b byte) types.PeerID {
	var id types.PeerID
	id[0] = b
	id[31] = b
	return id
}

func newSide(t *testing.T, net *fakeNet, id types.PeerID, tune func(*Options)) *side {
	s := &side{id: id, store: newMemStore(), dir: t.TempDir()}
	s.start(t, net, tune)
	return s
}

func (s *side) start(t *testing.T, net *fakeNet, tune func(*Options)) {
	opt := Options{
		Store: s.store, Transport: fakeLink{net}, DownloadDir: func() string { return s.dir },
		InitialWindow: 8, MaxWindow: 64, MinRTO: 300 * time.Millisecond, StallAfter: 500 * time.Millisecond,
		Emit: func(i Info) { s.mu.Lock(); s.seen = append(s.seen, i); s.mu.Unlock() },
	}
	if tune != nil {
		tune(&opt)
	}
	s.svc = New(opt)
	net.set(s.id, s.svc)
	require.NoError(t, s.svc.Start())
	t.Cleanup(s.svc.Stop)
}

func randomFile(t *testing.T, dir, name string, size int) (string, []byte) {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, data, 0o644))
	return p, data
}

func pair(t *testing.T, tune func(*Options)) (*fakeNet, *side, *side) {
	net := &fakeNet{nodes: map[types.PeerID]*Service{}}
	return net, newSide(t, net, peerID(1), tune), newSide(t, net, peerID(2), tune)
}

func waitState(t *testing.T, s *Service, id string, want State, within time.Duration) Info {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		last, _ := s.Get(id)
		if last.State == want {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("transfer never became %s: %+v", want, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func offerTo(t *testing.T, from, to *side, path string) Offer {
	o, err := from.svc.Prepare(to.id, path)
	require.NoError(t, err)
	_, err = to.svc.OnOffer(from.id, o)
	require.NoError(t, err)
	return o
}

func TestTransferOverALossyLink(t *testing.T) {
	net, alice, bob := pair(t, nil)
	net.lossPct = 10
	src, data := randomFile(t, alice.dir, "photos.zip", 3*ChunkSize*50+123)
	o := offerTo(t, alice, bob, src)

	in, _ := bob.svc.Get(o.ID)
	assert.Equal(t, StateOffered, in.State)
	assert.Equal(t, "photos.zip", in.Name)
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)

	done := waitState(t, bob.svc, o.ID, StateDone, 30*time.Second)
	got, err := os.ReadFile(done.Path)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(data, got), "the received file differs")
	assert.Equal(t, filepath.Join(bob.dir, "photos.zip"), done.Path)
	assert.Equal(t, int64(len(data)), done.Done)
	matches, _ := filepath.Glob(filepath.Join(bob.dir, "*.part"))
	assert.Empty(t, matches, "the partial file is renamed, not left behind")
	if runtime.GOOS == "windows" {
		zone, err := os.ReadFile(done.Path + ":Zone.Identifier")
		require.NoError(t, err, "a received file carries the Mark of the Web")
		assert.Contains(t, string(zone), "ZoneId=3")
	}

	net.lossPct = 0
	waitState(t, alice.svc, o.ID, StateDone, 10*time.Second)
	// progress was reported along the way
	var progress int
	for _, e := range bob.events() {
		if e.ID == o.ID && e.State == StateActive && e.Done > 0 && e.Done < e.Size {
			progress++
		}
	}
	assert.Positive(t, progress)
}

func TestEmptyFile(t *testing.T) {
	_, alice, bob := pair(t, nil)
	src, _ := randomFile(t, alice.dir, "empty.txt", 0)
	o := offerTo(t, alice, bob, src)
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)
	done := waitState(t, bob.svc, o.ID, StateDone, 5*time.Second)
	st, err := os.Stat(done.Path)
	require.NoError(t, err)
	assert.Zero(t, st.Size())
}

// A relay (or anyone on the path) that flips bits gets its frames thrown away; the file still
// arrives intact because the receiver asks for those chunks again.
func TestAlteredFramesAreDroppedAndRefetched(t *testing.T) {
	net, alice, bob := pair(t, nil)
	var n atomic.Int64
	net.mutate = func(f []byte) []byte {
		if frameKind(f[1]) == kindChunk && n.Add(1)%5 == 0 {
			f[len(f)/2] ^= 0x40
		}
		return f
	}
	src, data := randomFile(t, alice.dir, "a.bin", 40*ChunkSize)
	o := offerTo(t, alice, bob, src)
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)
	done := waitState(t, bob.svc, o.ID, StateDone, 30*time.Second)
	got, _ := os.ReadFile(done.Path)
	assert.True(t, bytes.Equal(data, got))
	assert.Positive(t, bob.svc.Dropped("open"), "altered frames must fail to open")
}

// A sender that holds the key (or a bug on its side) cannot slip in data that is not the offered
// content: chunks are checked against the root from the offer, whatever key sealed them.
func TestChunksThatDoNotMatchTheRootAreRejected(t *testing.T) {
	net, alice, bob := pair(t, nil)
	src, data := randomFile(t, alice.dir, "doc.pdf", 10*ChunkSize)
	o := offerTo(t, alice, bob, src)
	key, _ := hex.DecodeString(o.Key)
	k, _ := deriveKeys(key)
	id, _ := idKey(o.ID)
	tree, err := HashFile(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	// sealed correctly, but the data is not chunk 3 / the proof is for another chunk
	net.down.Store(true) // the real sender stays silent until the forgeries are in
	_, err = bob.svc.Accept(o.ID)
	require.NoError(t, err)
	evil := bytes.Repeat([]byte{0xee}, ChunkSize)
	bob.svc.HandleFrame(seal(k.toReceiver, kindChunk, id, encodeChunk(3, tree.Proof(3), evil)))
	bob.svc.HandleFrame(seal(k.toReceiver, kindChunk, id, encodeChunk(3, tree.Proof(4), data[4*ChunkSize:5*ChunkSize])))
	bob.svc.HandleFrame(seal(k.toReceiver, kindChunk, id, encodeChunk(3, tree.Proof(3), data[3*ChunkSize:4*ChunkSize-1])))
	bob.svc.HandleFrame(seal(k.toReceiver, kindChunk, id, encodeChunk(999, tree.Proof(3), data[3*ChunkSize:4*ChunkSize])))
	assert.Equal(t, int64(2), bob.svc.Dropped("proof"))
	assert.Equal(t, int64(2), bob.svc.Dropped("chunk"))
	info, _ := bob.svc.Get(o.ID)
	assert.Zero(t, info.Done, "nothing unverified counts as received")

	// a frame for this transfer sealed with the direction's other key is not a chunk either
	bob.svc.HandleFrame(seal(k.toSender, kindChunk, id, encodeChunk(3, tree.Proof(3), data[3*ChunkSize:4*ChunkSize])))
	assert.Equal(t, int64(1), bob.svc.Dropped("open"))

	net.down.Store(false)
	done := waitState(t, bob.svc, o.ID, StateDone, 20*time.Second)
	got, _ := os.ReadFile(done.Path)
	assert.True(t, bytes.Equal(data, got))
}

func TestDeclineAndCancel(t *testing.T) {
	net, alice, bob := pair(t, nil)
	src, _ := randomFile(t, alice.dir, "x.bin", 4*ChunkSize)

	// declined: the sender hears of it
	o := offerTo(t, alice, bob, src)
	require.NoError(t, bob.svc.Decline(o.ID))
	waitState(t, alice.svc, o.ID, StateDeclined, 5*time.Second)
	_, err := bob.svc.Accept(o.ID)
	assert.Error(t, err, "a declined offer cannot be accepted any more")

	// the receiver cancels midway: the partial file goes, the sender stops
	big, _ := randomFile(t, alice.dir, "big.bin", 400*ChunkSize)
	o2 := offerTo(t, alice, bob, big)
	_, err = bob.svc.Accept(o2.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o2.ID); return i.Done > 0 }, 10*time.Second, time.Millisecond)
	net.hold.Store(true) // midway: no more chunks for now
	require.NoError(t, bob.svc.Cancel(o2.ID))
	waitState(t, alice.svc, o2.ID, StateCancelled, 5*time.Second)
	info, _ := bob.svc.Get(o2.ID)
	assert.Equal(t, StateCancelled, info.State)
	matches, _ := filepath.Glob(filepath.Join(bob.dir, "*"))
	assert.Empty(t, matches, "a cancelled download leaves nothing behind")

	// the sender cancels midway: the receiver stops too
	net.hold.Store(false)
	o3 := offerTo(t, alice, bob, big)
	_, err = bob.svc.Accept(o3.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o3.ID); return i.Done > 0 }, 10*time.Second, time.Millisecond)
	net.hold.Store(true)
	require.NoError(t, alice.svc.Cancel(o3.ID))
	waitState(t, bob.svc, o3.ID, StateCancelled, 5*time.Second)
	matches, _ = filepath.Glob(filepath.Join(bob.dir, "*"))
	assert.Empty(t, matches)

	// a receiver that missed the cancel learns of it from the answer to its next request
	net.hold.Store(false)
	o4 := offerTo(t, alice, bob, big)
	_, err = bob.svc.Accept(o4.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o4.ID); return i.Done > 0 }, 10*time.Second, time.Millisecond)
	net.hold.Store(true)
	net.noCancel.Store(true)
	require.NoError(t, alice.svc.Cancel(o4.ID))
	time.Sleep(100 * time.Millisecond)
	net.noCancel.Store(false)
	waitState(t, bob.svc, o4.ID, StateCancelled, 10*time.Second)
	assert.Error(t, bob.svc.Cancel("nope"))
}

func TestSourceChangedAfterTheOffer(t *testing.T) {
	_, alice, bob := pair(t, nil)
	src, _ := randomFile(t, alice.dir, "draft.txt", 3*ChunkSize)
	o := offerTo(t, alice, bob, src)
	// the sender's copy is edited before the receiver accepts; the sender forgets its tree (a restart)
	alice.svc.mu.Lock()
	for _, og := range alice.svc.out {
		og.tree = nil
	}
	alice.svc.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, os.WriteFile(src, bytes.Repeat([]byte("z"), 3*ChunkSize), 0o644))
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)
	info := waitState(t, bob.svc, o.ID, StateFailed, 10*time.Second)
	assert.Contains(t, info.Error, "no longer has")
	waitState(t, alice.svc, o.ID, StateFailed, 5*time.Second)
}

func TestLimits(t *testing.T) {
	_, alice, bob := pair(t, func(o *Options) {
		o.MaxSize = 10 * ChunkSize
		o.FreeSpace = func(string) (uint64, error) { return 5 * ChunkSize, nil }
	})
	big, _ := randomFile(t, alice.dir, "big.bin", 11*ChunkSize)
	_, err := alice.svc.Prepare(bob.id, big)
	assert.ErrorContains(t, err, "larger than")
	_, err = alice.svc.Prepare(bob.id, alice.dir)
	assert.Error(t, err, "a folder is no file")

	// an offer bigger than the receiver's limit cannot be accepted
	_, err = bob.svc.OnOffer(alice.id, Offer{ID: strings.Repeat("ab", idLen), Name: "huge.iso", Size: 1 << 40,
		Root: strings.Repeat("00", 32), Key: strings.Repeat("11", 32)})
	require.NoError(t, err)
	_, err = bob.svc.Accept(strings.Repeat("ab", idLen))
	assert.ErrorContains(t, err, "larger than")

	// nor one the disk has no room for
	mid, _ := randomFile(t, alice.dir, "mid.bin", 8*ChunkSize)
	o := offerTo(t, alice, bob, mid)
	_, err = bob.svc.Accept(o.ID)
	assert.ErrorContains(t, err, "disk space")
	info, _ := bob.svc.Get(o.ID)
	assert.Equal(t, StateOffered, info.State, "it can still be accepted once there is room")

	// broken offers are refused outright
	_, err = bob.svc.OnOffer(alice.id, Offer{ID: "zz", Size: 1})
	assert.Error(t, err)
	_, err = bob.svc.OnOffer(alice.id, Offer{ID: strings.Repeat("cd", idLen), Size: 1, Root: "00", Key: strings.Repeat("11", 32)})
	assert.Error(t, err)
}

func TestNamesFromTheSenderStayInTheDownloadFolder(t *testing.T) {
	_, alice, bob := pair(t, nil)
	src, data := randomFile(t, alice.dir, "payload.bin", 2*ChunkSize)
	o, err := alice.svc.Prepare(bob.id, src)
	require.NoError(t, err)
	o.Name = `..\..\..\Windows\System32\evil.dll`
	_, err = bob.svc.OnOffer(alice.id, o)
	require.NoError(t, err)
	_, err = bob.svc.Accept(o.ID)
	require.NoError(t, err)
	done := waitState(t, bob.svc, o.ID, StateDone, 10*time.Second)
	assert.Equal(t, filepath.Join(bob.dir, "evil.dll"), done.Path)
	got, _ := os.ReadFile(done.Path)
	assert.True(t, bytes.Equal(data, got))

	// a second file of the same name does not overwrite the first
	o2, err := alice.svc.Prepare(bob.id, src)
	require.NoError(t, err)
	o2.Name = "evil.dll"
	_, _ = bob.svc.OnOffer(alice.id, o2)
	_, err = bob.svc.Accept(o2.ID)
	require.NoError(t, err)
	done2 := waitState(t, bob.svc, o2.ID, StateDone, 10*time.Second)
	assert.Equal(t, filepath.Join(bob.dir, "evil (1).dll"), done2.Path)
}

// The peer goes away midway: the transfer stalls, says so, and finishes once the peer is back.
func TestResumesAfterTheLinkComesBack(t *testing.T) {
	net, alice, bob := pair(t, nil)
	src, data := randomFile(t, alice.dir, "movie.mkv", 300*ChunkSize)
	o := offerTo(t, alice, bob, src)
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o.ID); return i.Done > 0 }, 10*time.Second, 2*time.Millisecond)
	net.down.Store(true)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o.ID); return i.Stalled }, 10*time.Second, 20*time.Millisecond, "a dead link shows as stalled")
	before, _ := bob.svc.Get(o.ID)
	assert.Less(t, before.Done, before.Size)
	net.down.Store(false)
	done := waitState(t, bob.svc, o.ID, StateDone, 30*time.Second)
	assert.False(t, done.Stalled)
	got, _ := os.ReadFile(done.Path)
	assert.True(t, bytes.Equal(data, got))
}

// Both apps restart in the middle of a download: it continues where it was, without fetching the
// chunks it already had, and the sender serves the file again after checking it is unchanged.
func TestResumesAfterRestart(t *testing.T) {
	net, alice, bob := pair(t, nil)
	src, data := randomFile(t, alice.dir, "backup.tar", 500*ChunkSize+999)
	o := offerTo(t, alice, bob, src)
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o.ID); return i.Done > 100*ChunkSize }, 20*time.Second, time.Millisecond)

	net.down.Store(true)
	time.Sleep(100 * time.Millisecond) // frames under way land
	bob.svc.Stop()
	alice.svc.Stop()
	mid, _ := bob.svc.Get(o.ID)
	require.Less(t, mid.Done, mid.Size, "the test needs an unfinished download")

	net.chunks.Store(0)
	net.down.Store(false)
	alice.start(t, net, nil)
	bob.start(t, net, nil)
	resumed, err := bob.svc.Get(o.ID)
	require.NoError(t, err)
	assert.Equal(t, StateActive, resumed.State)
	assert.Positive(t, resumed.Done, "the chunks written before the restart are kept")

	done := waitState(t, bob.svc, o.ID, StateDone, 30*time.Second)
	got, _ := os.ReadFile(done.Path)
	assert.True(t, bytes.Equal(data, got))
	need := int64(NumChunks(int64(len(data)))) - resumed.Done/ChunkSize
	assert.Less(t, net.chunks.Load(), need+need/2+64, "only the missing chunks are fetched again")
	waitState(t, alice.svc, o.ID, StateDone, 10*time.Second)
}

// A partial file damaged on disk while the app was closed does not end up as the received file:
// the final check catches it and the file is fetched again.
func TestDamagedPartialFileIsFetchedAgain(t *testing.T) {
	net, alice, bob := pair(t, nil)
	src, data := randomFile(t, alice.dir, "db.sqlite", 200*ChunkSize)
	o := offerTo(t, alice, bob, src)
	_, err := bob.svc.Accept(o.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { i, _ := bob.svc.Get(o.ID); return i.Done > 20*ChunkSize }, 20*time.Second, time.Millisecond)
	net.down.Store(true)
	time.Sleep(100 * time.Millisecond)
	bob.svc.Stop()

	recs, _ := loadRecords(bob.store)
	require.Len(t, recs, 1)
	part := recs[0].Part
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(bytes.Repeat([]byte{0}, 100), 5)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	net.down.Store(false)
	bob.start(t, net, nil)
	done := waitState(t, bob.svc, o.ID, StateDone, 30*time.Second)
	got, _ := os.ReadFile(done.Path)
	assert.True(t, bytes.Equal(data, got), "the damaged chunk must not survive")
}
