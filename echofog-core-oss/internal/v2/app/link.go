package app

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
)

const (
	maxLinkText = 16 << 10 // objects.maxLink: a longer link is never valid
	webMarker   = "/join/#join"
	trimTail    = ".,;:!?)]}>\"'»…"
)

// ExtractLink finds the first EchoFog link in pasted text (a chat message, a web page): the
// echofog:// form or the https join page. It returns the link without surrounding punctuation.
// Work is bounded by the text length: a white-space separated token longer than a link is skipped
// without being searched.
func ExtractLink(text string) (string, bool) {
	i := 0
	for i < len(text) {
		r, n := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			i += n
			continue
		}
		j := i
		for j < len(text) {
			r, n := utf8.DecodeRuneInString(text[j:])
			if unicode.IsSpace(r) {
				break
			}
			j += n
		}
		if j-i <= maxLinkText+64 {
			if l, ok := linkInToken(text[i:j]); ok {
				return l, true
			}
		}
		i = j
	}
	return "", false
}

func linkInToken(tok string) (string, bool) {
	for off := 0; off < len(tok); off++ {
		rest := tok[off:]
		switch {
		case strings.HasPrefix(rest, "echofog://"):
		case strings.HasPrefix(rest, "https://") && strings.Contains(rest, webMarker):
		default:
			continue
		}
		rest = strings.TrimRight(rest, trimTail)
		if len(rest) > maxLinkText || rest == "echofog:" || rest == "echofog:/" || rest == "echofog://" {
			return "", false
		}
		return rest, true
	}
	return "", false
}

// LinkInfo is what a link shows before joining. It never carries the access secret.
type LinkInfo struct {
	Kind    string // "contact" | "invitation"
	Node    string
	Expires time.Time
}

func (a *App) open(text string) (info LinkInfo, link string, node, device [32]byte, err error) {
	link, ok := ExtractLink(text)
	if !ok {
		return info, "", node, device, links.ErrNotLink
	}
	pv, err := a.cfg.Links.Open(link)
	if err != nil {
		return info, "", node, device, err
	}
	info = LinkInfo{Kind: pv.Kind.String(), Node: hex.EncodeToString(pv.Node[:]), Expires: pv.NotAfter}
	return info, link, pv.Node, pv.Device, nil
}

// Preview reads a link offline: nothing is dialed.
func (a *App) Preview(text string) (LinkInfo, error) {
	info, _, _, _, err := a.open(text)
	return info, err
}

// AddByLink joins by a link and saves the contact. A link to this node, to a blocked contact or a
// link that does not open is refused before anything is dialed; the session that comes back must
// be with the node and the device of the link.
func (a *App) AddByLink(ctx context.Context, text string) (Contact, error) {
	info, link, node, device, err := a.open(text)
	if err != nil {
		return Contact{}, err
	}
	if node == a.cfg.Self {
		return Contact{}, ErrSelf
	}
	a.cmu.Lock()
	old, oerr := a.loadContact(info.Node)
	a.cmu.Unlock()
	if oerr == nil && old.Blocked {
		return Contact{}, ErrBlocked
	}
	peer, err := a.cfg.Links.Redeem(ctx, link)
	if err != nil {
		return Contact{}, err
	}
	if [32]byte(peer.Node) != node || [32]byte(peer.Device) != device {
		return Contact{}, errors.New("app: the session is not with the node of the link")
	}
	a.cmu.Lock()
	defer a.cmu.Unlock()
	rec, err := a.loadContact(info.Node)
	switch {
	case errors.Is(err, ErrNoContact):
		rec = contactRec{Alias: "Контакт " + info.Node[:6], Added: a.cfg.Clock.Now().Unix()}
	case err != nil:
		return Contact{}, err
	case rec.Blocked:
		return Contact{}, ErrBlocked
	}
	rec.Device = hex.EncodeToString(device[:])
	if err := a.saveContact(info.Node, rec); err != nil {
		return Contact{}, err
	}
	return rec.contact(info.Node), nil
}

// UserMessage is the Russian text of an error for the UI, in a neutral form. It never repeats the
// internal error text.
func UserMessage(err error) string {
	switch {
	case errors.Is(err, ErrSelf):
		return "Это ссылка на этот же узел"
	case errors.Is(err, ErrBlocked):
		return "Контакт заблокирован"
	case errors.Is(err, ErrAlias):
		return "Имя должно быть непустым, не длиннее 64 символов и без служебных знаков"
	case errors.Is(err, ErrNoContact):
		return "Такого контакта нет"
	case errors.Is(err, ErrMode):
		return "Неизвестный режим доступа"
	case errors.Is(err, ErrNotHidden):
		return "Приглашения создаёт только скрытый узел"
	case errors.Is(err, ErrRequest):
		return "Неверные параметры: проверьте сроки и число входов"
	case errors.Is(err, ErrNoPass):
		return "Такого пропуска нет"
	case errors.Is(err, node.ErrNoDomesticEntry):
		return "В вашей стране нет подходящего входного узла: подключение по этой сети невозможно"
	case errors.Is(err, node.ErrRouteUnsupported):
		return "Эта сеть пока не поддерживается без проверенного входного узла"
	case errors.Is(err, ErrRunning):
		return "Сеть уже запущена"
	case errors.Is(err, links.ErrNotLink), errors.Is(err, links.ErrOtherVersion), errors.Is(err, links.ErrMismatch),
		errors.Is(err, links.ErrExpired), errors.Is(err, links.ErrRevoked), errors.Is(err, links.ErrNoContactLink),
		errors.Is(err, links.ErrNoInviter):
		return links.UserMessage(err)
	default:
		return "Не удалось выполнить действие"
	}
}
