package app

import (
	"context"
	"time"
)

// ExtractLink finds the first EchoFog link in pasted text (a chat message, a web page): the
// echofog:// form or the https join page. It returns the link without surrounding words.
func ExtractLink(text string) (string, bool) { return "", false }

// LinkInfo is what a link shows before joining. It never carries the access secret.
type LinkInfo struct {
	Kind    string // "contact" | "invitation"
	Node    string
	Expires time.Time
}

// Preview reads a link offline.
func (a *App) Preview(text string) (LinkInfo, error) { return LinkInfo{}, errNotImplemented }

// AddByLink joins by a link and saves the contact.
func (a *App) AddByLink(ctx context.Context, text string) (Contact, error) {
	return Contact{}, errNotImplemented
}

// UserMessage is the Russian text of an error for the UI.
func UserMessage(err error) string { return "" }
