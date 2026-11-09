package node

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/stretchr/testify/require"
)

func listSigner(t *testing.T, seed byte) *keys.Signer {
	t.Helper()
	s, err := keys.NewEd25519Signer(bytes.Repeat([]byte{seed}, 32))
	require.NoError(t, err)
	return s
}

// TestV2BootstrapManifestDeliversSignedList is the end-to-end path of T5.6 point 2: a release
// manifest carries a signed entry list; the update flow writes it next to node.json and applies
// it only after the list signature, term and seq pass. A forged, unsigned or older list is
// refused and both the applied list and the old update file stay as they were.
func TestV2BootstrapManifestDeliversSignedList(t *testing.T) {
	signer := listSigner(t, 7)
	cosigner := listSigner(t, 8)
	third := listSigner(t, 9)
	foreign := listSigner(t, 10)

	// The manifest needs two of the three compiled release keys; the list needs only one (the
	// 0.3.0 decision). The signer's key is the one the list is sealed with.
	oldKeys := bootstrap.ReleaseKeys
	bootstrap.ReleaseKeys = []keys.AKey{signer.Public()}
	t.Cleanup(func() { bootstrap.ReleaseKeys = oldKeys })

	p := newV2Peer(t, "bootmanifest")
	p.api.v2h.ReleaseKeys = []keys.AKey{signer.Public(), cosigner.Public(), third.Public()}
	p.start()

	p.api.v2l.Lock()
	s := p.api.v2s
	p.api.v2l.Unlock()
	require.NotNil(t, s)
	require.NotNil(t, s.rt.Release(), "the test build has release keys")

	m := s.rt.Release()
	now := time.Now()
	released := uint64(now.Unix())
	notAfter := uint64(now.Add(30 * 24 * time.Hour).Unix())

	offer := func(at uint64, list []byte) error {
		raw, err := objects.SealRelease(signer, at+30*86400, &pb.Release{
			Version: "0.3.1", ReleasedAt: at, BootstrapList: list,
		}, cosigner)
		require.NoError(t, err)
		_, err = m.Offer(raw)
		return err
	}
	sealList := func(s *keys.Signer, seq uint64, text string) []byte {
		raw, err := bootstrap.SealList(s, seq, notAfter, text)
		require.NoError(t, err)
		return raw
	}

	first := sealList(signer, 5, bootLine+"\n")
	require.NoError(t, offer(released, first))
	st, err := p.api.GetBootstrapNodes()
	require.NoError(t, err)
	require.Equal(t, uint64(5), st.Seq)
	require.Len(t, st.BuiltIn, 1)
	require.Equal(t, bootLine, st.BuiltIn[0].Link)

	// A candidate file exists; a refused list must not replace it.
	updatePath := filepath.Join(p.api.v2Dir(), v2BootstrapUpdate)
	oldFile := []byte("the previous candidate stays")
	require.NoError(t, os.WriteFile(updatePath, oldFile, 0o600))

	require.NoError(t, offer(released+1, sealList(foreign, 6, bootLine+"\n")), "the manifest is valid")
	require.NoError(t, offer(released+2, sealList(signer, 4, bootLine+"\n")), "the manifest is valid")
	require.NoError(t, offer(released+3, []byte("not a signed list")), "the manifest is valid")
	st, err = p.api.GetBootstrapNodes()
	require.NoError(t, err)
	require.Equal(t, uint64(5), st.Seq, "a refused list changed the applied seq")
	require.Len(t, st.BuiltIn, 1)
	kept, err := os.ReadFile(updatePath)
	require.NoError(t, err)
	require.Equal(t, oldFile, kept, "a refused list replaced the old update file")

	// A newer, properly signed list is written, applied and consumed.
	require.NoError(t, offer(released+4, sealList(signer, 7, bootLine+"\n")))
	st, err = p.api.GetBootstrapNodes()
	require.NoError(t, err)
	require.Equal(t, uint64(7), st.Seq)
	_, err = os.Stat(updatePath)
	require.True(t, os.IsNotExist(err), "the applied update file was not consumed")
}
