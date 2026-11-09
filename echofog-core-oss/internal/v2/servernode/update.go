package servernode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/release"
	"github.com/echofog/echofog-core/internal/v2/releasekeys"
)

var (
	// ErrDowngrade: the offered version is not newer than the running one.
	ErrDowngrade = errors.New("servernode: the offered version is not newer; rolling back is not allowed")
	// ErrBelowMin: the offered version is below the minimum version of the signed manifest.
	ErrBelowMin = errors.New("servernode: the offered version is below the minimum version of the signed manifest")
	// ErrNoReleaseKeys: this build has no release keys, so it cannot verify an update.
	ErrNoReleaseKeys = errors.New("servernode: this build has no release keys")
	// ErrNotConfirmed: the operator did not agree.
	ErrNotConfirmed = errors.New("servernode: the update was not confirmed")
	// ErrUpdateFailed: the new binary did not start; the old one was put back.
	ErrUpdateFailed = errors.New("servernode: the new binary did not start; the old one was restored")
	// ErrDevBuild: a development build is never updated.
	ErrDevBuild = errors.New("servernode: a development build is not updated")
	// ErrNoArtifact: the manifest has no file for this platform.
	ErrNoArtifact = errors.New("servernode: the manifest has no artifact for this platform")
	// ErrArtifact: the artifact of the manifest cannot be used.
	ErrArtifact = errors.New("servernode: bad update artifact")
)

// Build-time and test seams of the update command. ReleaseKeys are the release public keys of the
// build (filled by init from releaseKeysSpec; empty in an unofficial build).
var (
	ReleaseKeys      []keys.AKey
	UpdateHTTPClient *http.Client
	UpdateExePath    string
	BuildVersion     = "dev"
	UpdateSelfTest   func(ctx context.Context, exe string) (string, error)
	// UpdateNow replaces the wall clock that dates manifests (tests); nil is time.Now.
	UpdateNow func() time.Time
)

// releaseKeysSpec is the build-time release key list: Ed25519 public keys separated by commas in
// hex or base64. A release build passes it with
//
//	-ldflags "-X github.com/echofog/echofog-core/internal/v2/servernode.releaseKeysSpec=..."
//
// init parses it into ReleaseKeys; an empty spec is an OSS build with no release keys.
var releaseKeysSpec string

// buildReleaseKeys is the key set parsed by init, kept apart from ReleaseKeys so that tests can
// check the build wiring without depending on test mutations of ReleaseKeys.
var buildReleaseKeys []keys.AKey

func init() {
	buildReleaseKeys = releasekeys.MustParseSpec("servernode", releaseKeysSpec)
	ReleaseKeys = buildReleaseKeys
}

// maxArtifactSize bounds a download: a node binary is far smaller.
const maxArtifactSize = 256 << 20

// UpdateInfo describes an update that the signed manifest offers.
type UpdateInfo struct {
	Version  string
	Artifact string
	URL      string
	Size     uint64
	SHA256   []byte
	NotesURL string
	// MinVersion is the manifest's lower bound ("" = none).
	MinVersion string
	// Rollback marks an offer below the running version (only with UpdateDeps.AllowRollback).
	Rollback bool
}

// UpdateDeps are the inputs of the update.
type UpdateDeps struct {
	// Manager verifies manifests against the release keys of the build (2 of 3, T4.5).
	Manager        *release.Manager
	HTTP           *http.Client
	ManifestURL    string
	CurrentVersion string
	// Platform is "<goos>-<goarch>", the platform field of the manifest's artifacts.
	Platform string
	ExePath  string
	// AllowRollback lets the update install a version below the running one (never below min_version).
	AllowRollback bool
	// Confirm is the operator's consent. Nil means no consent: nothing is installed.
	Confirm func(info UpdateInfo) bool
	// SelfTest runs the binary at exe and returns the version it reports; nil runs "exe version".
	SelfTest func(ctx context.Context, exe string) (version string, err error)
}

// parseVersion reads X.Y.Z (with an optional leading v).
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') || len(p) > 6 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func cmpVersion(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// CheckUpdate fetches the manifest and returns the update for this platform, or nil when the
// running version is the offered one. It downloads no artifact. The manifest must carry two
// signatures of the build's release keys, and an older version is refused.
func CheckUpdate(ctx context.Context, d UpdateDeps) (*UpdateInfo, error) {
	if d.Manager == nil {
		return nil, ErrNoReleaseKeys
	}
	cur, ok := parseVersion(d.CurrentVersion)
	if !ok {
		return nil, ErrDevBuild
	}
	if _, err := d.Manager.FetchSite(ctx, d.HTTP, d.ManifestURL); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	last := d.Manager.Last()
	if last == nil || last.Release == nil {
		return nil, nil
	}
	rel := last.Release
	offered, ok := parseVersion(rel.GetVersion())
	if !ok {
		return nil, fmt.Errorf("%w: the manifest has version %q", ErrArtifact, rel.GetVersion())
	}
	// The signed lower bound comes first and no flag overrides it: below it a version is revoked.
	if m := rel.GetMinVersion(); m != "" {
		mv, ok := parseVersion(m)
		if !ok {
			return nil, fmt.Errorf("%w: the manifest has min_version %q", ErrArtifact, m)
		}
		if cmpVersion(offered, mv) < 0 {
			return nil, ErrBelowMin
		}
	}
	rollback := false
	switch c := cmpVersion(offered, cur); {
	case c < 0:
		if !d.AllowRollback {
			return nil, ErrDowngrade
		}
		rollback = true
	case c == 0:
		return nil, nil
	}
	for _, a := range rel.GetArtifacts() {
		if a.GetPlatform() == d.Platform {
			return &UpdateInfo{Version: strings.TrimPrefix(rel.GetVersion(), "v"), Artifact: a.GetName(), URL: a.GetUrl(), Size: a.GetSize(),
				SHA256: append([]byte(nil), a.GetSha256()...), NotesURL: rel.GetNotesUrl(), MinVersion: strings.TrimPrefix(rel.GetMinVersion(), "v"), Rollback: rollback}, nil
		}
	}
	return nil, ErrNoArtifact
}

func defaultSelfTest(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version").Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("the binary printed no version")
	}
	return strings.TrimPrefix(f[len(f)-1], "v"), nil
}

// ApplyUpdate downloads, verifies and installs an update after the operator confirmed it. The
// download is checked against the size and SHA-256 of the signed manifest and run once before the
// swap; the old binary stays beside the new one as <exe>.previous and is put back when the new one
// does not start. It installs a newer version only; a rollback needs UpdateDeps.AllowRollback and
// never goes below the signed min_version.
func ApplyUpdate(ctx context.Context, d UpdateDeps, info UpdateInfo) error {
	cur, ok := parseVersion(d.CurrentVersion)
	if !ok {
		return ErrDevBuild
	}
	offered, ok := parseVersion(info.Version)
	if !ok {
		return fmt.Errorf("%w: version %q", ErrArtifact, info.Version)
	}
	if info.MinVersion != "" {
		mv, ok := parseVersion(info.MinVersion)
		if !ok {
			return fmt.Errorf("%w: min_version %q", ErrArtifact, info.MinVersion)
		}
		if cmpVersion(offered, mv) < 0 {
			return ErrBelowMin
		}
	}
	switch c := cmpVersion(offered, cur); {
	case c == 0:
		return ErrDowngrade
	case c < 0 && !(d.AllowRollback && info.Rollback):
		return ErrDowngrade
	}
	u, err := url.Parse(info.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: https URL required", ErrArtifact)
	}
	if info.Size == 0 || info.Size > maxArtifactSize || len(info.SHA256) != sha256.Size || d.ExePath == "" || d.HTTP == nil {
		return fmt.Errorf("%w: size, hash or target", ErrArtifact)
	}
	if d.Confirm == nil || !d.Confirm(info) {
		return ErrNotConfirmed
	}
	test := d.SelfTest
	if test == nil {
		test = defaultSelfTest
	}

	tmp, err := download(ctx, d.HTTP, info, d.ExePath)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // gone after the rename; a leftover only if something failed before it
	if v, err := test(ctx, tmp); err != nil || strings.TrimPrefix(v, "v") != info.Version {
		return fmt.Errorf("%w: the downloaded binary reported %q (%v)", ErrArtifact, v, err)
	}

	previous := d.ExePath + ".previous"
	_ = os.Remove(previous)
	if err := os.Rename(d.ExePath, previous); err != nil {
		return fmt.Errorf("keep the previous binary: %w", err)
	}
	if err := os.Rename(tmp, d.ExePath); err != nil {
		_ = os.Rename(previous, d.ExePath)
		return fmt.Errorf("install the new binary: %w", err)
	}
	if v, err := test(ctx, d.ExePath); err != nil || strings.TrimPrefix(v, "v") != info.Version {
		if rerr := os.Rename(previous, d.ExePath); rerr != nil {
			return fmt.Errorf("%w; and the old binary could not be put back (%v): it is at %s", ErrUpdateFailed, rerr, previous)
		}
		return ErrUpdateFailed
	}
	return nil
}

// download fetches the artifact into a private temporary file next to the target and checks its
// size and hash. Redirects must stay on https.
func download(ctx context.Context, hc *http.Client, info UpdateInfo, exe string) (string, error) {
	c := *hc
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 3 {
			return fmt.Errorf("%w: redirect", ErrArtifact)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrArtifact, err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > int64(info.Size) {
		return "", fmt.Errorf("%w: status %d, length %d", ErrArtifact, resp.StatusCode, resp.ContentLength)
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	tmp := exe + ".tmp" + hex.EncodeToString(rnd[:])
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, int64(info.Size)+1))
	if err != nil {
		return fail(err)
	}
	if uint64(n) != info.Size {
		return fail(fmt.Errorf("%w: %d bytes, the manifest says %d", ErrArtifact, n, info.Size))
	}
	if !bytes.Equal(h.Sum(nil), info.SHA256) {
		return fail(fmt.Errorf("%w: the hash does not match the signed manifest", ErrArtifact))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}
