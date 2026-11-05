package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// cardDoc is the JSON form of one signed card: the exact envelope plus a human-readable summary.
// `dial` and `echo --card` only need the envelope; the summary is there for the operator.
type cardDoc struct {
	Scope      string   `json:"scope"`
	Seq        uint64   `json:"seq"`
	NotAfter   uint64   `json:"not_after"`
	NodeID     string   `json:"node_id"`
	DeviceID   string   `json:"device_id"`
	Envelope   string   `json:"envelope"`
	Addresses  []string `json:"addresses,omitempty"`
	ServiceIDs []string `json:"services,omitempty"`
}

func newCardDoc(ic *cards.IssuedCard, c *objects.Card) cardDoc {
	doc := cardDoc{
		Scope:    c.Scope.String(),
		Seq:      ic.Seq,
		NotAfter: ic.NotAfter,
		NodeID:   hex.EncodeToString(c.NodeID[:]),
		DeviceID: hex.EncodeToString(c.DeviceID[:]),
		Envelope: base64.StdEncoding.EncodeToString(ic.Envelope),
	}
	for _, a := range c.Addrs() {
		doc.Addresses = append(doc.Addresses, fmt.Sprintf("%s %s:%d", suiteName(a.GetSuite()), a.GetHost(), a.GetPort()))
	}
	for _, s := range c.Services() {
		for _, v := range s.GetVersions() {
			doc.ServiceIDs = append(doc.ServiceIDs, fmt.Sprintf("%#x/%d", s.GetId(), v))
		}
	}
	return doc
}

func suiteName(s uint32) string {
	switch s {
	case objects.SuiteNoise3:
		return "noise3"
	case objects.SuiteReality2QUIC:
		return "reality2-quic"
	case objects.SuiteReality2TCP:
		return "reality2-tcp"
	case objects.SuiteWS2:
		return "ws2"
	}
	return fmt.Sprintf("suite-%d", s)
}

func writeCardFile(path string, doc cardDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func readCardFile(path string) (cardDoc, error) {
	var doc cardDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("card file %s: %w", path, err)
	}
	if doc.Envelope == "" {
		return doc, fmt.Errorf("card file %s: no envelope", path)
	}
	return doc, nil
}

// verifyCardDoc decodes and verifies the signed card. `wantDevice`, when set, must match the
// card signer: the network addresses devices, so dialing another device's card is an error.
func verifyCardDoc(doc cardDoc, wantDevice string, now time.Time) (*objects.Card, []byte, error) {
	raw, err := base64.StdEncoding.DecodeString(doc.Envelope)
	if err != nil {
		return nil, nil, fmt.Errorf("card envelope: %w", err)
	}
	c, err := objects.VerifyCard(raw, now)
	if err != nil {
		return nil, nil, fmt.Errorf("card: %w", err)
	}
	if doc.DeviceID != "" && doc.DeviceID != hex.EncodeToString(c.DeviceID[:]) {
		return nil, nil, fmt.Errorf("card file: device id %s does not match the envelope %x", doc.DeviceID, c.DeviceID)
	}
	if wantDevice != "" {
		want, err := parseDeviceID(wantDevice)
		if err != nil {
			return nil, nil, err
		}
		if [32]byte(c.DeviceID) != [32]byte(want) {
			return nil, nil, fmt.Errorf("card is for device %x, not %s", c.DeviceID, wantDevice)
		}
	}
	return c, raw, nil
}

func parseDeviceID(s string) ([32]byte, error) {
	var id [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return id, fmt.Errorf("device id %q: want 64 hex characters", s)
	}
	copy(id[:], b)
	return id, nil
}

func knownCardPath(dataDir, device string) string {
	return filepath.Join(dataDir, knownDirName, device+".json")
}

func saveKnownCard(dataDir, device string, doc cardDoc) error {
	return writeCardFile(knownCardPath(dataDir, device), doc)
}
