package filetransfer

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bigFile writes size pseudo-random bytes (fast to make, impossible to compress) and returns the
// path and their SHA-256.
func bigFile(t *testing.T, dir, name string, size int64) (string, [32]byte) {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	require.NoError(t, err)
	defer f.Close()
	var seed [32]byte
	seed[0] = byte(size)
	src := rand.NewChaCha8(seed)
	h := sha256.New()
	_, err = io.CopyN(io.MultiWriter(f, h), src, size)
	require.NoError(t, err)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return p, sum
}

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func fileNode(t *testing.T, name string, tune func(*config.AppConfig)) (*coreNode.Node, *node.NodeApi) {
	dl := t.TempDir()
	return startNodeWith(t, name, func(c *config.AppConfig) {
		c.Storage.DownloadDir = dl
		if tune != nil {
			tune(c)
		}
	})
}

// waitOffer waits for the file message from the sender and returns the transfer ID it names.
func waitOffer(t *testing.T, inbox <-chan *dto.MessageDTO) string {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m := <-inbox:
			if m.IsMine || m.Type != dto.MsgTypeFile {
				continue
			}
			assert.NotContains(t, m.Metadata, "file_key_hex", "the file key never reaches the UI")
			var meta struct {
				Custom struct {
					Fid string `json:"fid"`
				} `json:"custom"`
			}
			require.NoError(t, json.Unmarshal([]byte(m.Metadata), &meta))
			require.NotEmpty(t, meta.Custom.Fid, "a file message names its transfer: %s", m.Metadata)
			return meta.Custom.Fid
		case <-deadline:
			t.Fatal("the file offer never arrived")
		}
	}
}

func waitTransfer(t *testing.T, api *node.NodeApi, id, state string, within time.Duration) dto.FileTransferDTO {
	t.Helper()
	var last dto.FileTransferDTO
	require.Eventually(t, func() bool {
		last, _ = api.GetFileTransfer(id)
		return last.State == state
	}, within, 50*time.Millisecond, "transfer never became %s: %+v", state, last)
	return last
}

// befriend makes from a contact of to: files from strangers are not offered.
func befriend(t *testing.T, from, to *node.NodeApi) {
	t.Helper()
	require.NoError(t, to.NewContact(dto.ContactDTO{PeerID: from.GetMyID(), PubKey: from.GetMyPubKey(), Alias: "friend"}))
}

func transfer(t *testing.T, from, to *node.NodeApi, src string, sum [32]byte, size int64) dto.FileTransferDTO {
	t.Helper()
	befriend(t, from, to)
	inbox := to.MessageHandler()
	// events arrive on goroutines of their own
	var (
		progressMu sync.Mutex
		progress   []dto.FileTransferDTO
	)
	stop := to.OnFileTransfer(func(d dto.FileTransferDTO) {
		progressMu.Lock()
		progress = append(progress, d)
		progressMu.Unlock()
	})
	defer stop()

	sent, err := from.SendFile(to.GetMyPubKey(), src)
	require.NoError(t, err)
	assert.Equal(t, "offered", sent.State)
	assert.Equal(t, size, sent.Size)

	id := waitOffer(t, inbox)
	require.Equal(t, sent.ID, id)
	offered := waitTransfer(t, to, id, "offered", 5*time.Second)
	assert.Equal(t, filepath.Base(src), offered.Name)

	start := time.Now()
	_, err = to.AcceptFile(id)
	require.NoError(t, err)
	done := waitTransfer(t, to, id, "done", 3*time.Minute)
	el := time.Since(start)
	t.Logf("%d MB in %s: %.1f MB/s", size>>20, el.Round(time.Millisecond), float64(size)/el.Seconds()/(1<<20))

	assert.Equal(t, size, done.Done)
	assert.Equal(t, sum, fileSum(t, done.Path), "the received file differs from the sent one")
	waitTransfer(t, from, id, "done", 10*time.Second)
	path, err := to.FileTransferPath(id)
	require.NoError(t, err)
	assert.Equal(t, done.Path, path)
	return done
}

// A few hundred MB between two nodes connected directly over loopback QUIC.
func TestFileTransferDirect(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 300 MB")
	}
	aN, alice := fileNode(t, "files-alice", nil)
	bN, bob := fileNode(t, "files-bob", nil)
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bN.Transport.Addr().String(), bN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	const size = 300<<20 + 12345
	src, sum := bigFile(t, t.TempDir(), "dataset.bin", size)
	done := transfer(t, alice, bob, src, sum, size)
	assert.Equal(t, bN.Cfg.Storage.DownloadDir, filepath.Dir(done.Path))
	assert.NotNil(t, aN.Swarm.GetPeer(bN.ID), "the nodes stayed directly connected")
}

// Two peers that cannot reach each other directly send a file through a relay: the relay carries
// every byte and can read none of it.
func TestFileTransferThroughRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 200 MB")
	}
	noDirect := func(c *config.AppConfig) {
		c.Network.NoAutoDial = true
		c.Network.HolePunch = false
	}
	rN, r := startNodeWith(t, "files-relay", func(c *config.AppConfig) {
		c.Relay.Server.Enabled = true
		c.Network.NoAutoDial = true
	})
	aN, alice := fileNode(t, "files-alice", noDirect)
	bN, bob := fileNode(t, "files-bob", noDirect)
	// they stand for peers behind NATs that do not let each other in: on one machine they would
	// connect directly, so each refuses the other
	aN.Swarm.BanPeer(bN.ID)
	bN.Swarm.BanPeer(aN.ID)
	join := network.JoinPeerAddr(rN.Transport.Addr().String(), rN.ID)
	require.NoError(t, alice.ConnectToPeer(join))
	require.NoError(t, bob.ConnectToPeer(join))
	require.Eventually(t, func() bool { return len(r.GetPeers()) >= 2 }, 5*time.Second, 50*time.Millisecond)

	const size = 200<<20 + 777
	src, sum := bigFile(t, t.TempDir(), "archive.7z", size)
	before := rN.RelayServer.Stats().BytesRouted
	transfer(t, alice, bob, src, sum, size)

	assert.Nil(t, aN.Swarm.GetPeer(bN.ID), "the peers never connected directly")
	routed := rN.RelayServer.Stats().BytesRouted - before
	assert.GreaterOrEqual(t, routed, uint64(size), "every byte went through the relay")

}

// The receiver's app restarts in the middle of a download; it continues from where it was.
func TestFileTransferResumesAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 120 MB")
	}
	dl := t.TempDir()
	dbBob := t.TempDir() + "/db"
	bobCfg := func(c *config.AppConfig) {
		c.Storage.DownloadDir = dl
		c.Storage.DatabasePath = dbBob
	}
	aN, alice := fileNode(t, "files-alice", nil)
	bN, bob := startNodeWith(t, "files-bob", bobCfg)
	require.NoError(t, bob.ConnectToPeer(network.JoinPeerAddr(aN.Transport.Addr().String(), aN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)

	const size = 120 << 20
	src, sum := bigFile(t, t.TempDir(), "video.mp4", size)
	inbox := bob.MessageHandler()
	befriend(t, alice, bob)
	sent, err := alice.SendFile(bob.GetMyPubKey(), src)
	require.NoError(t, err)
	id := waitOffer(t, inbox)
	require.Equal(t, sent.ID, id)
	waitTransfer(t, bob, id, "offered", 5*time.Second)
	_, err = bob.AcceptFile(id)
	require.NoError(t, err)
	require.Eventually(t, func() bool { d, _ := bob.GetFileTransfer(id); return d.Done > size/4 }, time.Minute, 5*time.Millisecond)

	bN.Stop()
	// the same profile starts again
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Network.BootstrapNodes = nil
	bobCfg(cfg)
	b2 := coreNode.NewNode(t.Context(), cfg, "files-bob-2")
	require.NoError(t, b2.Login([]byte("pass123")))
	require.NoError(t, b2.Start())
	t.Cleanup(b2.Stop)
	bob2 := node.NewNodeApi(b2)
	resumed, err := bob2.GetFileTransfer(id)
	require.NoError(t, err)
	assert.Equal(t, "active", resumed.State)
	assert.Positive(t, resumed.Done, "what arrived before the restart is kept")
	t.Logf("resumed at %d of %d MB", resumed.Done>>20, size>>20)

	require.NoError(t, bob2.ConnectToPeer(network.JoinPeerAddr(aN.Transport.Addr().String(), aN.ID)))
	done := waitTransfer(t, bob2, id, "done", 2*time.Minute)
	assert.Equal(t, sum, fileSum(t, done.Path))
	matches, _ := filepath.Glob(filepath.Join(dl, "*.part"))
	assert.Empty(t, matches)
}

// Either side can call a transfer off midway; nothing is left on the receiver's disk.
func TestFileTransferCancelAndDecline(t *testing.T) {
	_, alice := fileNode(t, "files-alice", nil)
	bN, bob := fileNode(t, "files-bob", nil)
	require.NoError(t, alice.ConnectToPeer(network.JoinPeerAddr(bN.Transport.Addr().String(), bN.ID)))
	require.Eventually(t, func() bool { return len(alice.GetPeers()) > 0 && len(bob.GetPeers()) > 0 }, 5*time.Second, 50*time.Millisecond)
	inbox := bob.MessageHandler()

	small, _ := bigFile(t, t.TempDir(), "notes.txt", 5000)
	befriend(t, alice, bob)
	sent, err := alice.SendFile(bob.GetMyPubKey(), small)
	require.NoError(t, err)
	id := waitOffer(t, inbox)
	require.Equal(t, sent.ID, id)
	waitTransfer(t, bob, id, "offered", 5*time.Second)
	require.NoError(t, bob.DeclineFile(id))
	waitTransfer(t, alice, id, "declined", 10*time.Second)

	const size = 400 << 20
	big, _ := bigFile(t, t.TempDir(), "disk.img", size)
	sent, err = alice.SendFile(bob.GetMyPubKey(), big)
	require.NoError(t, err)
	id = waitOffer(t, inbox)
	require.Equal(t, sent.ID, id)
	waitTransfer(t, bob, id, "offered", 5*time.Second)
	_, err = bob.AcceptFile(id)
	require.NoError(t, err)
	require.Eventually(t, func() bool { d, _ := bob.GetFileTransfer(id); return d.Done > 0 }, 30*time.Second, 2*time.Millisecond)
	require.NoError(t, alice.CancelFile(id))
	got := waitTransfer(t, bob, id, "cancelled", 10*time.Second)
	assert.Less(t, got.Done, int64(size))
	require.Eventually(t, func() bool {
		left, _ := filepath.Glob(filepath.Join(bN.Cfg.Storage.DownloadDir, "*"))
		return len(left) == 0
	}, 5*time.Second, 50*time.Millisecond, "the partial download is deleted")
	_, err = bob.FileTransferPath(id)
	assert.Error(t, err)
}
