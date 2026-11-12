package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// v2BootstrapUpdate is the file the release update flow drops next to node.json: a signed list
// that replaces the build's official list when its seq is newer (T5.6).
const v2BootstrapUpdate = "bootstrap-update.bin"

// openV2Bootstrap builds the entry-list manager of the profile: the signed list of the build, the
// applied update and the user's entries from node.json and its T5.5 file.
func openV2Bootstrap(dir string, set bootstrapSetting, kv store.KV, now func() time.Time, log *slog.Logger) *bootstrap.Manager {
	custom := append([]string(nil), set.Entries...)
	if set.Path != "" {
		path := set.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if info, err := os.Stat(path); err == nil && info.Size() <= bootstrap.MaxListBytes {
			if raw, err := os.ReadFile(path); err == nil {
				if es, err := bootstrap.Parse(string(raw), now()); err == nil {
					for _, e := range es {
						custom = append(custom, e.Line)
					}
				} else {
					warn(log, "v2 bootstrap: the list file is not usable", err)
				}
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			warn(log, "v2 bootstrap: the list file is not usable", err)
		}
	}
	return bootstrap.OpenManager(bootstrap.ManagerConfig{
		Builtin: bootstrap.BuiltinRaw(), Keys: bootstrap.ReleaseKeys, Custom: custom, KV: kv,
		Now: now, Log: log,
		SaveCustom: func(lines []string) error { return saveV2Bootstrap(dir, lines) },
	})
}

func warn(log *slog.Logger, msg string, err error) {
	if log != nil {
		log.Warn(msg, "err", err)
	}
}

// saveV2Bootstrap writes the user's entries back to node.json, keeping every other setting.
func saveV2Bootstrap(dir string, lines []string) error {
	return updateV2Settings(dir, func(set *v2Settings) {
		set.Bootstrap.Entries = append([]string(nil), lines...)
	})
}

// v2BootstrapManager returns the list manager of the running stack, or nil.
func (n *NodeApi) v2BootstrapManager() *bootstrap.Manager {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	if n.v2s == nil || n.v2s.boot == nil {
		return nil
	}
	return n.v2s.boot
}

// v2SessionDevices is the set of devices with a live session.
func (n *NodeApi) v2SessionDevices() map[[32]byte]bool {
	n.v2l.Lock()
	s := n.v2s
	n.v2l.Unlock()
	out := map[[32]byte]bool{}
	if s == nil || s.rt == nil {
		return out
	}
	for _, sess := range s.rt.Status().Sessions {
		out[[32]byte(sess.Device)] = true
	}
	return out
}

// bootstrapNodeOf maps one list entry to the UI row. The address is shown to the user who asked
// for the screen; it never goes to the log.
func bootstrapNodeOf(o bootstrap.Origin, connected map[[32]byte]bool) BootstrapNode {
	e := o.Entry
	n := BootstrapNode{Link: e.Line, Builtin: o.Builtin, Index: o.Index, Enabled: o.Enabled}
	switch e.Kind {
	case bootstrap.KindCard:
		n.Kind = "card"
		if e.Card != nil {
			n.PeerID = hex.EncodeToString(e.Card.DeviceID[:])
			n.Connected = connected[e.Card.DeviceID]
		}
	case bootstrap.KindDevice:
		n.Kind = "device"
		n.PeerID = hex.EncodeToString(e.Device[:])
		n.Connected = connected[e.Device]
		n.Addr = addrText(e)
	case bootstrap.KindAddr:
		n.Kind = "addr"
		n.Addr = addrText(e)
	}
	return n
}

func addrText(e bootstrap.Entry) string {
	if e.Addr == nil {
		return ""
	}
	return net.JoinHostPort(e.Addr.GetHost(), strconv.Itoa(int(e.Addr.GetPort())))
}

// bootstrapErr is the Russian text of a list error; the internal text (which may name a line)
// stays out of the UI.
func bootstrapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bootstrap.ErrListStale):
		return &v2UserError{msg: "Список узлов входа не новее установленного: обновление не применено"}
	case errors.Is(err, bootstrap.ErrListSignature):
		return &v2UserError{msg: "Подпись списка узлов входа не проверена: обновление не применено"}
	case errors.Is(err, bootstrap.ErrListExpired):
		return &v2UserError{msg: "Срок списка узлов входа истёк: обновление не применено"}
	case errors.Is(err, bootstrap.ErrList):
		return &v2UserError{msg: "Не получилось прочитать узел входа: проверьте ссылку или строку списка"}
	}
	return err
}

// SetBootstrapNodeEnabled switches one official entry on or off (protocol 2 only).
func (n *NodeApi) SetBootstrapNodeEnabled(index int, on bool) error {
	m := n.v2BootstrapManager()
	if m == nil {
		return ErrV2NotBuilt
	}
	return bootstrapErr(m.SetBuiltinEnabled(index, on))
}

// CheckBootstrapNode dials one entry and reports whether it answered. The entry line, the address
// and the keys stay out of the log.
func (n *NodeApi) CheckBootstrapNode(entry string) (bool, error) {
	n.v2l.Lock()
	s := n.v2s
	n.v2l.Unlock()
	if s == nil || s.rt == nil {
		return false, ErrV2NotBuilt
	}
	es, err := bootstrap.Parse(strings.TrimSpace(entry), s.rt.Clock().Now())
	if err != nil || len(es) != 1 {
		return false, bootstrapErr(fmt.Errorf("%w: entry", bootstrap.ErrList))
	}
	svc := bootstrap.New(s.rt, es, s.rt.Clock().Now).Log(n.Logger)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	return svc.Check(ctx, es[0]), nil
}

// ApplyBootstrapList offers a signed official list that came with a release manifest (`update`,
// T4.5). Without a valid release signature, a term or a newer seq it is refused and nothing
// changes.
func (n *NodeApi) ApplyBootstrapList(raw []byte) error {
	m := n.v2BootstrapManager()
	if m == nil {
		return ErrV2NotBuilt
	}
	return bootstrapErr(m.Offer(raw))
}

// applyBootstrapList delivers the signed entry list of a release manifest (T5.6). The list is
// checked first (signature, term, seq): a refused list leaves the applied list and the old update
// file untouched. An accepted list is written next to node.json and consumed by OfferFile, so the
// startup path and the update path use the same rules. Only counts and the error class are logged.
func (s *v2Stack) applyBootstrapList(raw []byte) {
	if len(raw) == 0 || s.boot == nil || s.dir == "" {
		return
	}
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	if err := s.boot.Check(raw); err != nil {
		if !errors.Is(err, bootstrap.ErrListStale) && s.log != nil {
			s.log.Warn("v2 bootstrap: the signed list in the manifest was refused", "err", err)
		}
		return
	}
	path := filepath.Join(s.dir, v2BootstrapUpdate)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		warn(s.log, "v2 bootstrap: writing the signed list update", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		warn(s.log, "v2 bootstrap: placing the signed list update", err)
		return
	}
	applied, err := s.boot.OfferFile(path)
	if err != nil {
		warn(s.log, "v2 bootstrap: the signed list update was refused", err)
		return
	}
	if applied && s.log != nil {
		s.log.Info("v2 bootstrap: signed list updated", "seq", s.boot.Seq())
	}
}

// runBootstrapEntries dials the entries once in the background, without logging their lines.
func (n *NodeApi) runBootstrapEntries(entries []bootstrap.Entry) {
	n.v2l.Lock()
	s := n.v2s
	n.v2l.Unlock()
	if s == nil || s.rt == nil || len(entries) == 0 {
		return
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		rep := bootstrap.New(s.rt, entries, s.rt.Clock().Now).Log(n.Logger).Run(ctx)
		if n.Logger != nil {
			n.Logger.Info("v2 bootstrap finished", "entries", len(entries), "joined", rep.Joined, "failed", rep.Failed)
		}
	}()
}

// ---- protocol 2 network backend: the bootstrap screen (T5.6) --------------------------------

func (b *v2NetBackend) GetBootstrapNodes() (BootstrapStatus, error) {
	m := b.api.v2BootstrapManager()
	if m == nil {
		return BootstrapStatus{}, ErrV2NotBuilt
	}
	st := BootstrapStatus{Protocol: 2, Seq: m.Seq(), Expires: int64(m.NotAfter()),
		BuiltIn: []BootstrapNode{}, Custom: []BootstrapNode{}}
	connected := b.api.v2SessionDevices()
	for _, o := range m.Builtin() {
		st.BuiltIn = append(st.BuiltIn, bootstrapNodeOf(o, connected))
	}
	for _, o := range m.Custom() {
		st.Custom = append(st.Custom, bootstrapNodeOf(o, connected))
	}
	st.Peers = len(b.app.NetStatus().Peers)
	return st, nil
}

// AddBootstrapNode saves one of the user's entries and dials it at once.
func (b *v2NetBackend) AddBootstrapNode(entry string) error {
	m := b.api.v2BootstrapManager()
	if m == nil {
		return ErrV2NotBuilt
	}
	e, err := m.AddCustom(entry)
	if err != nil {
		return bootstrapErr(err)
	}
	b.api.runBootstrapEntries([]bootstrap.Entry{e})
	return nil
}

// RemoveBootstrapNode forgets one of the user's entries.
func (b *v2NetBackend) RemoveBootstrapNode(entry string) error {
	m := b.api.v2BootstrapManager()
	if m == nil {
		return ErrV2NotBuilt
	}
	return bootstrapErr(m.RemoveCustom(entry))
}

var _ netBackend = (*v2NetBackend)(nil)
