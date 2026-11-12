package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	coreNode "github.com/echofog/echofog-core/internal/v2/node"
)

// CountryStatus is the "Country" setting of protocol 2 (V2-FIELD-1). Code is the saved choice:
// "" is automatic (GeoIP of the own public address), "none" is "do not specify" (the unknown row
// of the route policy), and a two-letter code is the user's own answer. Source and Detected come
// from the running node: source is auto, set, hint or unknown, detected is the country the node
// would use. Without a running node only Code is known.
type CountryStatus struct {
	Protocol int    `json:"protocol"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Detected string `json:"detected,omitempty"`
}

// GetCountry reports the country setting. At protocol 1 it only returns Protocol 1: the setting
// belongs to protocol 2.
func (n *NodeApi) GetCountry() (CountryStatus, error) {
	st := CountryStatus{Protocol: 1}
	if n.node == nil || n.node.Cfg == nil {
		return st, errors.New("node not started")
	}
	if !n.node.Cfg.IsV2() {
		return st, nil
	}
	st.Protocol = 2
	dir := n.v2Dir()
	if dir == "" {
		return st, errors.New("v2 settings: no profile directory")
	}
	set, err := loadV2Settings(dir)
	if err != nil {
		return st, err
	}
	st.Code = set.Country
	n.v2l.Lock()
	s := n.v2s
	n.v2l.Unlock()
	if s != nil && s.rt != nil {
		cc, src := s.rt.OwnCountry()
		st.Source, st.Detected = string(src), cc
	}
	return st, nil
}

// SetCountry saves the country choice and applies it to the running node at once. "" is automatic,
// "none" means "do not specify a country", and anything else must be a two-letter code.
func (n *NodeApi) SetCountry(choice string) error {
	if n.node == nil || n.node.Cfg == nil {
		return errors.New("node not started")
	}
	if !n.node.Cfg.IsV2() {
		return ErrV2NotBuilt
	}
	choice = strings.ToLower(strings.TrimSpace(choice))
	if choice != "" && choice != coreNode.CountryNone && !isCountryCode(choice) {
		return &v2UserError{msg: "Страна указывается кодом из двух латинских букв или «не указывать»"}
	}
	if isCountryCode(choice) {
		choice = strings.ToUpper(choice)
	}
	dir := n.v2Dir()
	if err := updateV2Settings(dir, func(s *v2Settings) { s.Country = choice }); err != nil {
		return err
	}
	n.v2l.Lock()
	s := n.v2s
	n.v2l.Unlock()
	if s != nil && s.rt != nil {
		s.rt.SetCountry(choice)
	}
	return nil
}

func isCountryCode(cc string) bool {
	if len(cc) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if cc[i] < 'a' || cc[i] > 'z' {
			return false
		}
	}
	return true
}

// updateV2Settings rewrites node.json with one setting changed, keeping everything else (the
// bootstrap entries, the listeners). The file is private and replaced whole.
func updateV2Settings(dir string, fn func(*v2Settings)) error {
	if dir == "" {
		return errors.New("v2 settings: no profile directory")
	}
	set, err := loadV2Settings(dir)
	if err != nil {
		return err
	}
	fn(&set)
	raw, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, v2SettingsFile), raw, 0o600); err != nil {
		return fmt.Errorf("v2 settings: %w", err)
	}
	return nil
}
