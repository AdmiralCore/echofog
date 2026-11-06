package app

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/store"
)

const (
	contactPrefix = "c/"
	maxAlias      = 64
	maxContacts   = 10000
)

// Contact is a person the user added.
type Contact struct {
	Node    string // hex node ID
	Device  string // hex device ID of the device the link led to
	Alias   string
	Added   time.Time
	Blocked bool
}

type contactRec struct {
	Device  string `json:"d"`
	Alias   string `json:"a"`
	Added   int64  `json:"t"`
	Blocked bool   `json:"b,omitempty"`
}

func (r contactRec) contact(node string) Contact {
	return Contact{Node: node, Device: r.Device, Alias: r.Alias, Added: time.Unix(r.Added, 0), Blocked: r.Blocked}
}

// maxContacts is the contact limit in force.
func (a *App) maxContacts() int {
	if a.cfg.MaxContacts > 0 {
		return a.cfg.MaxContacts
	}
	return maxContacts
}

// full reports whether the list holds the most contacts it may. The caller holds cmu.
func (a *App) full() (bool, error) {
	n := a.maxContacts()
	ents, err := a.cfg.Store.Scan([]byte(contactPrefix), n)
	if err != nil {
		return false, err
	}
	return len(ents) >= n, nil
}

func ckey(node string) []byte { return []byte(contactPrefix + strings.ToLower(node)) }

// loadContact reads one contact; the caller holds cmu for a read-modify-write.
func (a *App) loadContact(node string) (contactRec, error) {
	v, err := a.cfg.Store.Get(ckey(node))
	if errors.Is(err, store.ErrNotFound) {
		return contactRec{}, ErrNoContact
	}
	if err != nil {
		return contactRec{}, err
	}
	var r contactRec
	if err := json.Unmarshal(v, &r); err != nil {
		return contactRec{}, err
	}
	return r, nil
}

func (a *App) saveContact(node string, r contactRec) error {
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return a.cfg.Store.Put(ckey(node), v)
}

// Contacts lists contacts by alias.
func (a *App) Contacts() ([]Contact, error) {
	ents, err := a.cfg.Store.Scan([]byte(contactPrefix), a.maxContacts())
	if err != nil {
		return nil, err
	}
	out := make([]Contact, 0, len(ents))
	for _, e := range ents {
		var r contactRec
		if json.Unmarshal(e.Value, &r) != nil {
			continue // a damaged record is skipped; it must not hide the others
		}
		out = append(out, r.contact(strings.TrimPrefix(string(e.Key), contactPrefix)))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alias != out[j].Alias {
			return out[i].Alias < out[j].Alias
		}
		return out[i].Node < out[j].Node
	})
	return out, nil
}

func cleanAlias(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxAlias {
		return "", ErrAlias
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", ErrAlias
		}
	}
	return s, nil
}

func (a *App) update(node string, fn func(*contactRec)) error {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	r, err := a.loadContact(node)
	if err != nil {
		return err
	}
	fn(&r)
	return a.saveContact(node, r)
}

// SetAlias renames a contact.
func (a *App) SetAlias(node, alias string) error {
	alias, err := cleanAlias(alias)
	if err != nil {
		return err
	}
	return a.update(node, func(r *contactRec) { r.Alias = alias })
}

// DeleteContact removes a contact.
func (a *App) DeleteContact(node string) error {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	if _, err := a.loadContact(node); err != nil {
		return err
	}
	return a.cfg.Store.Delete(ckey(node))
}

// Block marks a contact blocked: a link to it is not followed.
func (a *App) Block(node string) error {
	return a.update(node, func(r *contactRec) { r.Blocked = true })
}

// Unblock lifts the mark.
func (a *App) Unblock(node string) error {
	return a.update(node, func(r *contactRec) { r.Blocked = false })
}
