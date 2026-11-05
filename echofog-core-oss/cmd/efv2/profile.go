package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// File layout of one efv2 profile. The identity file keeps the recovery phrase and the device
// secrets; the v2 store keeps sessions, cards and the abuse log (T4.3).
const (
	identityFileName = "efv2-identity.json"
	nodeFileName     = "efv2-node.json"
	cardsDirName     = "efv2-cards"
	knownDirName     = "efv2-known"
	statusFileName   = "efv2-status.json"
	fileVersion      = 1
)

// identityFile is the on-disk profile of a lab node. It is written with mode 0600 and never
// logged. The phrase is kept in plaintext because efv2 has no OS protector; production profiles
// use the v1 storage key and secret store (T4.1/T4.3).
type identityFile struct {
	Version     int    `json:"version"`
	Mnemonic    string `json:"mnemonic"`
	DeviceSeed  string `json:"device_seed"`
	HsPrivate   string `json:"hs_private"`
	MaskPrivate string `json:"mask_private"`

	NodeID   string `json:"node_id"`
	DeviceID string `json:"device_id"`

	CreatedAt          int64  `json:"created_at"`
	DeviceListSeq      uint64 `json:"device_list_seq"`
	DeviceListNotAfter uint64 `json:"device_list_not_after"`
}

// nodeFile keeps the profile name and the last listener specification, so `card` can rebuild the
// same card when the node is not running.
type nodeFile struct {
	Version int      `json:"version"`
	Profile string   `json:"profile"`
	Listen  []string `json:"listen,omitempty"`
	Cover   string   `json:"cover,omitempty"`
	// Bootstrap is the path of the bootstrap list (T5.5).
	Bootstrap string `json:"bootstrap,omitempty"`
}

func identityPath(dataDir string) string { return filepath.Join(dataDir, identityFileName) }
func nodePath(dataDir string) string     { return filepath.Join(dataDir, nodeFileName) }
func statusPath(dataDir string) string   { return filepath.Join(dataDir, statusFileName) }

func loadIdentityFile(dataDir string) (*identityFile, error) {
	raw, err := os.ReadFile(identityPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is not an efv2 profile: run `efv2 init --data %s` first", dataDir, dataDir)
		}
		return nil, err
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("identity file %s: %w", identityPath(dataDir), err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("identity file %s: version %d, want %d", identityPath(dataDir), f.Version, fileVersion)
	}
	return &f, nil
}

func saveIdentityFile(dataDir string, f *identityFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writePrivate(identityPath(dataDir), raw)
}

func loadNodeFile(dataDir string) (*nodeFile, error) {
	raw, err := os.ReadFile(nodePath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s has no %s: run `efv2 init` first", dataDir, nodeFileName)
		}
		return nil, err
	}
	var f nodeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("node file %s: %w", nodePath(dataDir), err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("node file %s: version %d, want %d", nodePath(dataDir), f.Version, fileVersion)
	}
	if _, err := platformOf(f.Profile); err != nil {
		return nil, err
	}
	return &f, nil
}

func saveNodeFile(dataDir string, f *nodeFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(nodePath(dataDir), raw, 0o600)
}

func createIdentity(dataDir, profile string) (*identityFile, error) {
	platform, err := platformOf(profile)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(identityPath(dataDir)); err == nil {
		return nil, fmt.Errorf("%s already holds an identity", dataDir)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}

	phrase, err := identity.NewMnemonic(12)
	if err != nil {
		return nil, err
	}
	account, err := identity.FromMnemonic(phrase, "")
	if err != nil {
		return nil, err
	}
	defer account.Destroy()
	device, err := identity.NewDevice()
	if err != nil {
		return nil, err
	}
	defer device.Destroy()

	seed, err := deviceSeed(device)
	if err != nil {
		return nil, err
	}
	hs, err := privateKey(device.Handshake())
	if err != nil {
		return nil, err
	}
	mask, err := privateKey(device.Masking())
	if err != nil {
		return nil, err
	}

	now := time.Now()
	nodeID, deviceID := account.NodeID(), device.ID()
	f := &identityFile{
		Version:            fileVersion,
		Mnemonic:           phrase,
		DeviceSeed:         hex.EncodeToString(seed),
		HsPrivate:          hex.EncodeToString(hs),
		MaskPrivate:        hex.EncodeToString(mask),
		NodeID:             hex.EncodeToString(nodeID[:]),
		DeviceID:           hex.EncodeToString(deviceID[:]),
		CreatedAt:          now.Unix(),
		DeviceListSeq:      1,
		DeviceListNotAfter: uint64(now.Add(identity.DeviceListLifetime).Unix()),
	}
	nf := &nodeFile{Version: fileVersion, Profile: string(platform)}

	// Create the store before the files are written: a profile whose store cannot be opened is
	// not a usable profile. The storage key comes from the account, so it is always the same.
	prof := node.DefaultProfile(platform)
	st, err := store.Open(store.Options{
		Dir:      store.Path(dataDir),
		Key:      account.StorageKey(),
		MemCapMB: prof.MemCapMB,
	})
	if err != nil {
		return nil, fmt.Errorf("create the v2 store: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	if err := saveIdentityFile(dataDir, f); err != nil {
		return nil, err
	}
	if err := saveNodeFile(dataDir, nf); err != nil {
		return nil, err
	}
	return f, nil
}

// deriveIdentity rebuilds the account and device from the profile file and verifies the stored
// public identifiers, so a swapped or damaged identity is refused before the store is opened.
func deriveIdentity(f *identityFile) (*identity.Account, *identity.Device, error) {
	account, err := identity.FromMnemonic(f.Mnemonic, "")
	if err != nil {
		return nil, nil, fmt.Errorf("identity: %w", err)
	}
	seed, err := hex.DecodeString(f.DeviceSeed)
	if err != nil {
		account.Destroy()
		return nil, nil, fmt.Errorf("identity: device seed: %w", err)
	}
	hs, err := hex.DecodeString(f.HsPrivate)
	if err != nil {
		account.Destroy()
		return nil, nil, fmt.Errorf("identity: hs key: %w", err)
	}
	mask, err := hex.DecodeString(f.MaskPrivate)
	if err != nil {
		account.Destroy()
		return nil, nil, fmt.Errorf("identity: mask key: %w", err)
	}
	device, err := identity.DeviceFromKeys(seed, hs, mask)
	if err != nil {
		account.Destroy()
		return nil, nil, fmt.Errorf("identity: %w", err)
	}
	nodeID := account.NodeID()
	devID := device.ID()
	if f.NodeID != "" && f.NodeID != hex.EncodeToString(nodeID[:]) {
		account.Destroy()
		device.Destroy()
		return nil, nil, fmt.Errorf("identity file: node id does not match the recovery phrase")
	}
	if f.DeviceID != "" && f.DeviceID != hex.EncodeToString(devID[:]) {
		account.Destroy()
		device.Destroy()
		return nil, nil, fmt.Errorf("identity file: device id does not match the device keys")
	}
	return account, device, nil
}

func platformOf(name string) (node.Platform, error) {
	switch name {
	case string(node.PlatformServer):
		return node.PlatformServer, nil
	case string(node.PlatformDesktop):
		return node.PlatformDesktop, nil
	case string(node.PlatformAndroid):
		return node.PlatformAndroid, nil
	default:
		return "", fmt.Errorf("profile %q: want server, desktop or android", name)
	}
}

func deviceSeed(d *identity.Device) ([]byte, error) {
	var out []byte
	err := d.WithSeed(func(b []byte) error {
		out = append([]byte(nil), b...)
		return nil
	})
	return out, err
}

func privateKey(k *identity.KeyPair) ([]byte, error) {
	var out []byte
	err := k.WithPrivate(func(b []byte) error {
		out = append([]byte(nil), b...)
		return nil
	})
	return out, err
}

func writePrivate(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
