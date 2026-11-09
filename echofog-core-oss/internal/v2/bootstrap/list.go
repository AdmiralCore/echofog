// The signed list of entry nodes (T5.6).
//
// The official list is a text of T5.5 lines wrapped in a signature of the release key set
// (T4.5/T12.4e). The wrapper is small and local to the distribution of the list; it is not a
// protocol frame:
//
//	"EFBL2\0" | seq u64be | not_after u64be | key_len u8 | signer akey | body_len u32be | body | sig
//
// The signature covers every byte before it. A list is accepted only when the signer is a current
// release key, the signature verifies, the term has not passed and the seq is newer than the one
// already applied. Anything else is refused and nothing changes.
package bootstrap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Signed list limits and errors.
const (
	ListMaxBytes = 64 << 10
	customMax    = 32
	listMagic    = "EFBL2\x00"
	listSigLen   = 64
	listKeyLen   = 33 // an Ed25519 akey: tag + 32 bytes
)

var (
	// ErrListSignature reports a list whose signature does not verify or whose signer is not a
	// release key.
	ErrListSignature = errors.New("bootstrap: list signature")
	// ErrListExpired reports a list past its not_after.
	ErrListExpired = errors.New("bootstrap: list expired")
	// ErrListStale reports a list whose seq is not newer than the applied one.
	ErrListStale = errors.New("bootstrap: list seq is not newer")
)

var (
	listKey = []byte("list")
	offKey  = []byte("off")
)

// SignedList is a verified signed list: the official entry nodes of a release.
type SignedList struct {
	Seq      uint64
	NotAfter uint64
	Signer   keys.AKey
	Entries  []Entry
	Raw      []byte
}

// SealList signs text with a release key. seq and not_after are the list's own sequence and term.
func SealList(s *keys.Signer, seq, notAfter uint64, text string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("bootstrap: no signer")
	}
	if seq == 0 || notAfter == 0 {
		return nil, errors.New("bootstrap: seq and not_after are required")
	}
	if len(text) > ListMaxBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrList, ListMaxBytes)
	}
	pub := s.Public()
	if _, err := keys.ParseKind(pub, keys.Ed25519); err != nil {
		return nil, fmt.Errorf("bootstrap: release key: %w", err)
	}
	body := []byte(text)
	out := make([]byte, 0, len(listMagic)+8+8+1+len(pub)+4+len(body)+listSigLen)
	out = append(out, listMagic...)
	out = binary.BigEndian.AppendUint64(out, seq)
	out = binary.BigEndian.AppendUint64(out, notAfter)
	out = append(out, byte(len(pub)))
	out = append(out, pub...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, body...)
	out = append(out, s.Sign(out)...)
	if len(out) > ListMaxBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrList, ListMaxBytes)
	}
	return out, nil
}

// OpenList verifies raw against the release key set and parses its entries.
func OpenList(raw []byte, release []keys.AKey, now time.Time) (*SignedList, error) {
	const head = len(listMagic) + 8 + 8 + 1
	if len(raw) < head+listKeyLen+4+listSigLen || len(raw) > ListMaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrList, len(raw))
	}
	if string(raw[:len(listMagic)]) != listMagic {
		return nil, fmt.Errorf("%w: not a signed list", ErrList)
	}
	p := len(listMagic)
	seq := binary.BigEndian.Uint64(raw[p:])
	notAfter := binary.BigEndian.Uint64(raw[p+8:])
	p += 16
	kl := int(raw[p])
	p++
	if kl != listKeyLen || len(raw) < p+kl+4 {
		return nil, fmt.Errorf("%w: signer key", ErrList)
	}
	signer, err := keys.ParseKind(raw[p:p+kl], keys.Ed25519)
	if err != nil {
		return nil, fmt.Errorf("%w: signer key: %v", ErrList, err)
	}
	p += kl
	bl := int(binary.BigEndian.Uint32(raw[p:]))
	p += 4
	if bl > ListMaxBytes || len(raw) != p+bl+listSigLen {
		return nil, fmt.Errorf("%w: lengths", ErrList)
	}
	body := raw[p : p+bl]
	sig := raw[p+bl:]
	trusted := false
	for _, k := range release {
		if k.Equal(signer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil, fmt.Errorf("%w: signer is not a release key", ErrListSignature)
	}
	if err := keys.Verify(signer, raw[:len(raw)-listSigLen], sig); false && err != nil {
		return nil, fmt.Errorf("%w: %v", ErrListSignature, err)
	}
	if notAfter == 0 || uint64(now.Unix()) >= notAfter+clockSkew {
		return nil, ErrListExpired
	}
	entries, err := Parse(string(body), now)
	if err != nil {
		return nil, err
	}
	return &SignedList{Seq: seq, NotAfter: notAfter, Signer: signer, Entries: entries, Raw: append([]byte(nil), raw...)}, nil
}

// clockSkew is the expiry tolerance, the same as in objects.
const clockSkew = 60

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	// Builtin is the signed list embedded at build time. Empty in the OSS source build.
	Builtin []byte
	// Keys is the release public key set. Empty: no signed list is trusted.
	Keys []keys.AKey
	// Custom is the user's own entries (node.json "bootstrap").
	Custom []string
	// KV persists the applied list and the disabled built-in entries. Nil: memory only.
	KV store.KV
	// Now is the clock.
	Now func() time.Time
	// Log receives diagnostics without list lines or addresses. Nil is silent.
	Log *slog.Logger
	// SaveCustom persists the user's entries (node.json). Called under the manager lock.
	SaveCustom func([]string) error
}

// Origin is one list entry with where it came from.
type Origin struct {
	Entry Entry
	// Builtin is true for an entry of the signed list; Index is then its 1-based number.
	Builtin bool
	Index   int
	Enabled bool
}

// Manager holds the official list and the user's entries.
type Manager struct {
	mu sync.Mutex

	cfg     ManagerConfig
	builtin *SignedList
	stored  *SignedList
	custom  []Entry
	lines   []string
	off     map[[32]byte]bool
	err     error
}

// OpenManager loads the built-in list, the applied update and the user's entries.
func OpenManager(cfg ManagerConfig) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	m := &Manager{cfg: cfg, off: map[[32]byte]bool{}}
	now := cfg.Now()
	if len(cfg.Builtin) > 0 {
		l, err := OpenList(cfg.Builtin, cfg.Keys, now)
		if err != nil {
			m.err = err
			m.warn("bootstrap: the built-in list is not usable", "err", err)
		} else {
			m.builtin = l
		}
	}
	if cfg.KV != nil {
		if raw, err := cfg.KV.Get(listKey); err == nil {
			if l, lerr := OpenList(raw, cfg.Keys, now); lerr == nil && m.newer(l) {
				m.stored = l
			} else if lerr != nil {
				m.warn("bootstrap: the stored list is not usable", "err", lerr)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			m.warn("bootstrap: reading the stored list", "err", err)
		}
		if raw, err := cfg.KV.Get(offKey); err == nil {
			var list []string
			if json.Unmarshal(raw, &list) == nil {
				for _, h := range list {
					b, err := hex.DecodeString(h)
					if err == nil && len(b) == 32 {
						m.off[[32]byte(b)] = true
					}
				}
			}
		}
	}
	for _, line := range cfg.Custom {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		es, err := Parse(line, now)
		if err != nil || len(es) != 1 {
			m.warn("bootstrap: a saved entry is not usable")
			continue
		}
		if m.hasCustom(es[0].Line) {
			continue
		}
		if len(m.custom) == customMax {
			m.warn("bootstrap: too many saved entries")
			break
		}
		m.custom = append(m.custom, es[0])
		m.lines = append(m.lines, es[0].Line)
	}
	return m
}

func (m *Manager) warn(msg string, args ...any) {
	if m.cfg.Log != nil {
		m.cfg.Log.Warn(msg, args...)
	}
}

// effective is the official list in force: the stored update when it is newer than the build's.
func (m *Manager) effective() *SignedList {
	if m.stored != nil && (m.builtin == nil || m.stored.Seq > m.builtin.Seq) {
		return m.stored
	}
	return m.builtin
}

func (m *Manager) newer(l *SignedList) bool {
	cur := m.effective()
	return cur == nil || l.Seq > cur.Seq
}

func (m *Manager) currentSeq() uint64 {
	if l := m.effective(); l != nil {
		return l.Seq
	}
	return 0
}

func digest(line string) [32]byte { return sha256.Sum256([]byte(line)) }

func (m *Manager) hasCustom(line string) bool {
	for _, l := range m.lines {
		if strings.EqualFold(l, line) {
			return true
		}
	}
	return false
}

// Builtin returns the official entries with their enabled state and 1-based numbers.
func (m *Manager) Builtin() []Origin {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.effective()
	if l == nil {
		return nil
	}
	out := make([]Origin, 0, len(l.Entries))
	for i, e := range l.Entries {
		e.Builtin, e.Index = true, i+1
		out = append(out, Origin{Entry: e, Builtin: true, Index: i + 1, Enabled: !m.off[digest(e.Line)]})
	}
	return out
}

// Custom returns the user's entries.
func (m *Manager) Custom() []Origin {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Origin, 0, len(m.custom))
	for _, e := range m.custom {
		out = append(out, Origin{Entry: e, Enabled: true})
	}
	return out
}

// Entries returns what is dialed: enabled official entries followed by the user's.
func (m *Manager) Entries() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	seen := map[string]bool{}
	if l := m.effective(); l != nil {
		for i, e := range l.Entries {
			if m.off[digest(e.Line)] {
				continue
			}
			e.Builtin, e.Index = true, i+1
			out = append(out, e)
			seen[strings.ToLower(e.Line)] = true
		}
	}
	for _, e := range m.custom {
		if seen[strings.ToLower(e.Line)] {
			continue
		}
		seen[strings.ToLower(e.Line)] = true
		out = append(out, e)
	}
	return out
}

// AddCustom parses and saves one user entry.
func (m *Manager) AddCustom(line string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	line = strings.TrimSpace(line)
	if line == "" {
		return Entry{}, fmt.Errorf("%w: empty entry", ErrList)
	}
	es, err := Parse(line, m.cfg.Now())
	if err != nil {
		return Entry{}, err
	}
	if len(es) != 1 {
		return Entry{}, fmt.Errorf("%w: one entry per line", ErrList)
	}
	if m.hasCustom(es[0].Line) {
		return es[0], nil
	}
	if len(m.custom) >= customMax {
		return Entry{}, fmt.Errorf("%w: more than %d entries", ErrList, customMax)
	}
	prevLines := append([]string(nil), m.lines...)
	prevCustom := append([]Entry(nil), m.custom...)
	m.custom = append(m.custom, es[0])
	m.lines = append(m.lines, es[0].Line)
	if err := m.saveLocked(); err != nil {
		m.custom, m.lines = prevCustom, prevLines
		return Entry{}, err
	}
	return es[0], nil
}

// RemoveCustom forgets one user entry.
func (m *Manager) RemoveCustom(line string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	line = strings.TrimSpace(line)
	if line == "" {
		return fmt.Errorf("%w: empty entry", ErrList)
	}
	if es, err := Parse(line, m.cfg.Now()); err == nil && len(es) == 1 {
		line = es[0].Line
	}
	idx := -1
	for i, l := range m.lines {
		if strings.EqualFold(l, line) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	prevLines := append([]string(nil), m.lines...)
	prevCustom := append([]Entry(nil), m.custom...)
	m.lines = append(append([]string(nil), m.lines[:idx]...), m.lines[idx+1:]...)
	m.custom = append(append([]Entry(nil), m.custom[:idx]...), m.custom[idx+1:]...)
	if err := m.saveLocked(); err != nil {
		m.custom, m.lines = prevCustom, prevLines
		return err
	}
	return nil
}

// SetBuiltinEnabled switches one official entry on or off and persists the choice.
func (m *Manager) SetBuiltinEnabled(index int, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.effective()
	if l == nil || index < 1 || index > len(l.Entries) {
		return fmt.Errorf("%w: no entry %d", ErrList, index)
	}
	d := digest(l.Entries[index-1].Line)
	prev := m.off[d]
	if on {
		delete(m.off, d)
	} else {
		m.off[d] = true
	}
	raw, _ := json.Marshal(m.offListLocked())
	if m.cfg.KV != nil {
		if err := m.cfg.KV.Put(offKey, raw); err != nil {
			if prev {
				m.off[d] = true
			} else {
				delete(m.off, d)
			}
			return fmt.Errorf("bootstrap: saving the choice: %w", err)
		}
	}
	return nil
}

func (m *Manager) offListLocked() []string {
	out := make([]string, 0, len(m.off))
	for d := range m.off {
		out = append(out, hex.EncodeToString(d[:]))
	}
	return out
}

func (m *Manager) saveLocked() error {
	if m.cfg.SaveCustom == nil {
		return nil
	}
	return m.cfg.SaveCustom(append([]string(nil), m.lines...))
}

// Offer applies a signed list from a release update. The signature, the term and the seq are
// checked; a rejected list changes nothing.
func (m *Manager) Offer(raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, err := OpenList(raw, m.cfg.Keys, m.cfg.Now())
	if err != nil {
		return err
	}
	if !m.newer(l) {
		return fmt.Errorf("%w: %d after %d", ErrListStale, l.Seq, m.currentSeq())
	}
	if m.cfg.KV != nil {
		if err := m.cfg.KV.Put(listKey, l.Raw); err != nil {
			return fmt.Errorf("bootstrap: saving the list: %w", err)
		}
	}
	m.stored = l
	return nil
}

// OfferFile applies a signed list from path, deleting it on success.
func (m *Manager) OfferFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Size() > ListMaxBytes {
		return false, fmt.Errorf("%w: the update file is too large", ErrList)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if err := m.Offer(raw); err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return true, err
	}
	return true, nil
}

// Seq returns the sequence of the effective official list, NotAfter its term.
func (m *Manager) Seq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentSeq()
}

// NotAfter returns the term of the effective official list.
func (m *Manager) NotAfter() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.effective(); l != nil {
		return l.NotAfter
	}
	return 0
}

// Err reports why the build's or the stored list was not usable, or nil.
func (m *Manager) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}
