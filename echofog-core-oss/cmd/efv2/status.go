package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
)

// statusDoc is the JSON snapshot that `run` writes on a timer and `status` prints. It is also what
// the test bed reads to check sessions, paths, standby paths and abuse counters.
type statusDoc struct {
	NodeID     string       `json:"node_id"`
	DeviceID   string       `json:"device_id"`
	Profile    string       `json:"profile"`
	Background string       `json:"background"`
	StartedAt  time.Time    `json:"started_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	Stale      bool         `json:"stale"`
	Sessions   []sessionDoc `json:"sessions"`
	Abuse      []abuseDoc   `json:"abuse"`
	// BirthdayTried and BirthdayOK count birthday punch attempts and the ones that found a path.
	BirthdayTried int64 `json:"birthday_tried"`
	BirthdayOK    int64 `json:"birthday_ok"`
}

type sessionDoc struct {
	NodeID           string    `json:"node_id"`
	DeviceID         string    `json:"device_id"`
	SID              string    `json:"sid"`
	Suite            uint16    `json:"suite"`
	Path             string    `json:"path"`
	Remote           string    `json:"remote,omitempty"`
	LocalDeviceID    string    `json:"local_device_id"`
	Paths            []uint64  `json:"paths"`
	ActivePath       uint64    `json:"active_path"`
	Standby          int       `json:"standby"`
	StreamsLocal     int       `json:"streams_local"`
	StreamsPeer      int       `json:"streams_peer"`
	LastActivity     time.Time `json:"last_activity"`
	GoingAway        bool      `json:"going_away"`
	GreaseSent       uint64    `json:"grease_sent"`
	DatagramsUnknown uint64    `json:"datagrams_unknown"`
	DatagramsBad     uint64    `json:"datagrams_bad"`
	DatagramsDropped uint64    `json:"datagrams_dropped"`
	Abuse            uint64    `json:"abuse"`
}

type abuseDoc struct {
	Source string `json:"source"`
	Count  uint64 `json:"count"`
}

func (a *app) statusDoc() statusDoc {
	st := a.rt.Status()
	doc := statusDoc{
		NodeID:        hex.EncodeToString(st.Node[:]),
		DeviceID:      hex.EncodeToString(st.Device[:]),
		Profile:       string(st.Profile.Platform),
		Background:    string(st.Background),
		StartedAt:     st.StartedAt,
		UpdatedAt:     time.Now().UTC(),
		Sessions:      make([]sessionDoc, 0, len(st.Sessions)),
		Abuse:         make([]abuseDoc, 0, len(st.Abuse)),
		BirthdayTried: st.Birthday.Tried,
		BirthdayOK:    st.Birthday.OK,
	}
	for _, s := range st.Sessions {
		remote := ""
		if s.Remote.IsValid() {
			remote = s.Remote.String()
		}
		doc.Sessions = append(doc.Sessions, sessionDoc{
			NodeID:           hex.EncodeToString(s.Node[:]),
			DeviceID:         hex.EncodeToString(s.Device[:]),
			SID:              hex.EncodeToString(s.SID[:]),
			Suite:            s.Suite,
			Path:             pathName(s.Path),
			Remote:           remote,
			LocalDeviceID:    hex.EncodeToString(s.LocalDevice[:]),
			Paths:            s.Paths,
			ActivePath:       s.ActivePath,
			Standby:          s.Standby,
			StreamsLocal:     s.StreamsLocal,
			StreamsPeer:      s.StreamsPeer,
			LastActivity:     s.LastActivity,
			GoingAway:        s.GoingAway,
			GreaseSent:       s.Stats.GreaseSent,
			DatagramsUnknown: s.Stats.DatagramsUnknown,
			DatagramsBad:     s.Stats.DatagramsBad,
			DatagramsDropped: s.Stats.DatagramsDropped,
			Abuse:            s.Abuse,
		})
	}
	for _, c := range st.Abuse {
		doc.Abuse = append(doc.Abuse, abuseDoc{
			Source: hex.EncodeToString(c.Source[:]),
			Count:  c.Count,
		})
	}
	return doc
}

// WriteStatus writes a snapshot atomically enough for a reader (small file, one writer).
func (a *app) WriteStatus() error {
	return writeStatusFile(statusPath(a.data), a.statusDoc())
}

func writeStatusFile(path string, doc statusDoc) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

func readStatusFile(path string) (statusDoc, error) {
	var doc statusDoc
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doc, fmt.Errorf("no status snapshot at %s: is `efv2 run` running?", path)
		}
		return doc, err
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("status snapshot %s: %w", path, err)
	}
	return doc, nil
}

// sessionFor returns the status of one device from a snapshot.
func sessionFor(st node.Status, device node.DeviceID) (node.SessionStatus, bool) {
	for _, s := range st.Sessions {
		if s.Device == device {
			return s, true
		}
	}
	return node.SessionStatus{}, false
}
