// Package browserprofiles keeps the ClientHello fingerprints of browsers and honest stand-ins
// (uTLS Chrome, curl, crypto/tls, quic-go), per country and network. The M1 detectors calibrate
// against them, and T2.6d uses the same file as a regression: a carrier whose hello drifts away
// from the Chrome family is a finding, not a taste.
package browserprofiles

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DBVersion is the version of the profile file, reported in every launchpad report.
const DBVersion = "2026.11.1"

const (
	SourceChrome     = "chrome"
	SourceUTLSChrome = "utls-chrome"
	SourceCurl       = "curl"
	SourceCryptoTLS  = "crypto/tls"
	SourceQUICGo     = "quic-go"
	SourceQUICChrome = "quic-go-chromeparrot"
)

// TLSProfile is one captured ClientHello.
type TLSProfile struct {
	JA3        string `json:"ja3"`
	JA4        string `json:"ja4"`
	Grease     bool   `json:"grease"`
	Ciphers    int    `json:"ciphers"`
	Extensions int    `json:"extensions"`
	ALPN       string `json:"alpn,omitempty"`
	Version    uint16 `json:"version"`
	PQ         bool   `json:"pq"`
	ECH        bool   `json:"ech"`
	Bytes      int    `json:"bytes"`
	SHA256     string `json:"sha256"`
}

// QUICProfile is one captured QUIC client Initial.
type QUICProfile struct {
	Version   uint32   `json:"version"`
	FirstSize int      `json:"first_size"`
	FixedBit  bool     `json:"fixed_bit"`
	Decrypted bool     `json:"decrypted"`
	JA4       string   `json:"ja4,omitempty"`
	SNI       string   `json:"sni,omitempty"`
	ALPN      []string `json:"alpn,omitempty"`
}

// Profile is one source in one country/network.
type Profile struct {
	ID            string       `json:"id"`
	Source        string       `json:"source"`
	SourceVersion string       `json:"source_version,omitempty"`
	Country       string       `json:"country"`
	Network       string       `json:"network"`
	CapturedAt    string       `json:"captured_at"`
	TLS           *TLSProfile  `json:"tls,omitempty"`
	QUIC          *QUICProfile `json:"quic,omitempty"`
	Note          string       `json:"note,omitempty"`
}

// Database is the file testbed/browserprofiles/profiles.json.
type Database struct {
	Version     string    `json:"version"`
	GeneratedAt string    `json:"generated_at"`
	Entries     []Profile `json:"entries"`
}

// Load reads a profile database.
func Load(path string) (*Database, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var db Database
	if err := json.Unmarshal(raw, &db); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if db.Version == "" {
		return nil, fmt.Errorf("%s: no version", path)
	}
	return &db, nil
}

// Save writes the database sorted by id, so a refresh produces a reviewable diff.
func (db *Database) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sort.Slice(db.Entries, func(i, j int) bool { return db.Entries[i].ID < db.Entries[j].ID })
	raw, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

// BrowserJA4 is the set of JA4 labels that count as browser-like for D4.
func (db *Database) BrowserJA4() map[string]bool {
	out := map[string]bool{}
	for _, p := range db.Entries {
		if !isBrowserSource(p.Source) {
			continue
		}
		if p.TLS != nil && p.TLS.JA4 != "" {
			out[p.TLS.JA4] = true
		}
		if p.QUIC != nil && p.QUIC.JA4 != "" {
			out[p.QUIC.JA4] = true
		}
	}
	return out
}

// BrowserJA3 is the JA3 counterpart, for reports and for older detectors.
func (db *Database) BrowserJA3() map[string]bool {
	out := map[string]bool{}
	for _, p := range db.Entries {
		if isBrowserSource(p.Source) && p.TLS != nil && p.TLS.JA3 != "" {
			out[p.TLS.JA3] = true
		}
	}
	return out
}

func isBrowserSource(s string) bool {
	switch s {
	case SourceChrome, SourceUTLSChrome, SourceQUICChrome:
		return true
	}
	return false
}

// LoadOrCapture loads the database, or captures what this machine can produce when it is missing.
func LoadOrCapture(ctx context.Context, path, country, network string) (*Database, error) {
	db, err := Load(path)
	if err == nil {
		return db, nil
	}
	db = CaptureLocal(ctx, country, network)
	if db == nil || len(db.Entries) == 0 {
		return &Database{Version: DBVersion, GeneratedAt: time.Now().UTC().Format(time.RFC3339)}, nil
	}
	_ = db.Save(path)
	return db, nil
}

// CaptureLocal records every source this machine has: the uTLS Chrome preset, quic-go with
// ChromeParrot, crypto/tls, curl, and a real Chrome when one is installed. It never pretends a
// stand-in is a browser: the source and its version are in the entry.
func CaptureLocal(ctx context.Context, country, network string) *Database {
	now := time.Now().UTC().Format(time.RFC3339)
	db := &Database{Version: DBVersion, GeneratedAt: now}
	add := func(p Profile, err error) {
		if err != nil {
			return
		}
		p.Country, p.Network, p.CapturedAt = country, network, now
		p.ID = fmt.Sprintf("%s-%s-%s", p.Source, country, network)
		db.Entries = append(db.Entries, p)
	}

	if p, err := captureTLSChrome(); err == nil {
		add(p, nil)
	}
	if p, err := captureQUICChrome(ctx); err == nil {
		add(p, nil)
	}
	if p, err := captureCryptoTLS(); err == nil {
		add(p, nil)
	}
	if p, err := captureCurl(ctx); err == nil {
		add(p, nil)
	}
	if p, err := captureChrome(ctx); err == nil {
		add(p, nil)
	}
	return db
}
