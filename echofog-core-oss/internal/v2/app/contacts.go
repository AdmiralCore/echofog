package app

import "time"

// Contact is a person the user added.
type Contact struct {
	Node    string // hex node ID
	Device  string // hex device ID of the device the link led to
	Alias   string
	Added   time.Time
	Blocked bool
}

// Contacts lists contacts by alias.
func (a *App) Contacts() ([]Contact, error) { return nil, errNotImplemented }

// SetAlias renames a contact.
func (a *App) SetAlias(node, alias string) error { return errNotImplemented }

// DeleteContact removes a contact.
func (a *App) DeleteContact(node string) error { return errNotImplemented }

// Block and Unblock mark a contact.
func (a *App) Block(node string) error   { return errNotImplemented }
func (a *App) Unblock(node string) error { return errNotImplemented }
