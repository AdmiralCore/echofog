package app

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
)

// NoticeKind names a notice the UI shows (T11.5): what the runtime reports, in the app's words.
type NoticeKind string

const (
	// NoticeUpdate: a newer release exists (a signed manifest says so). Nothing is downloaded.
	NoticeUpdate NoticeKind = "update"
	// NoticeNoDomesticEntry: the country policy has no entry node (T6.4).
	NoticeNoDomesticEntry NoticeKind = "no-domestic-entry"
	// NoticeReleaseUnavailable: the stored release data is damaged, so updates are not checked.
	NoticeReleaseUnavailable NoticeKind = "release-unavailable"
	// NoticeNoCover: this node has no camouflaged listener (D37).
	NoticeNoCover NoticeKind = "no-cover"
	// NoticeClockSkew: the device clock differs from the network's (T3.8-fix2).
	NoticeClockSkew NoticeKind = "clock-skew"
)

// eventClockSkew is the kind of the clock event of T3.8-fix2. That branch is not in this one, so
// the sink knows the kind by its name; the day node.EventClockSkew lands this constant goes.
const eventClockSkew node.EventKind = "node.clock-skew"

// ErrFreeQuotaUsed is what the exit refusal "free volume for today is used" maps to (T10.3).
var ErrFreeQuotaUsed = errors.New("app: free quota used")

// UpdateView is a release the UI offers. The manifest has no "security" flag, so Important means
// the build is below min_supported of a component: peers will stop accepting it.
type UpdateView struct {
	Version    string
	NotesURL   string // https only, "" when the manifest had none
	Important  bool
	Components []string
}

// Notice is one thing the UI is told. Text is Russian in a neutral form; Update is set for
// NoticeUpdate only.
type Notice struct {
	Kind   NoticeKind
	Text   string
	Update *UpdateView
}

// ErrorScreen is a full-screen error of the UI: a stable Code (the UI picks its English text by
// it) and the Russian Title and Text.
type ErrorScreen struct {
	Code  string
	Title string
	Text  string
}

var screens = map[string]ErrorScreen{
	"other-version": {"other-version", "Создано другой версией",
		"Эта ссылка создана версией EchoFog, которую приложение не поддерживает. Нужно обновить приложение или получить новую ссылку."},
	"no-cover": {"no-cover", "У сервера нет обложки",
		"Сервер не маскирует свой трафик, поэтому подключиться к нему нельзя. Нужно выбрать другой сервер."},
	"free-quota-used": {"free-quota-used", "Бесплатный объём на сегодня закончился",
		"Бесплатный трафик этого сервера на сегодня исчерпан и обновится завтра. Пока можно выбрать другой сервер."},
}

// ScreenByCode returns the error screen with that code.
func ScreenByCode(code string) (ErrorScreen, bool) {
	s, ok := screens[code]
	return s, ok
}

// Screen picks the error screen for an error, if one exists.
func Screen(err error) (ErrorScreen, bool) {
	var ref *exit.Refusal
	switch {
	case err == nil:
		return ErrorScreen{}, false
	case errors.Is(err, links.ErrOtherVersion):
		return screens["other-version"], true
	case errors.As(err, &ref) && ref.Reason == exit.ReasonNoCamouflage:
		return screens["no-cover"], true
	case errors.Is(err, ErrFreeQuotaUsed):
		return screens["free-quota-used"], true
	}
	return ErrorScreen{}, false
}

type subscriber struct {
	fn func(Notice)
}

// notices is the fan-out of the App: the subscribers and the newest update.
type notices struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	update *UpdateView
}

// Sink returns the sink to give node.Config.Events: it turns runtime events into notices.
func (a *App) Sink() node.EventSink { return node.EventFunc(a.onEvent) }

// Subscribe registers fn for notices; the returned function removes it. fn runs on the runtime's
// goroutine and must not block.
func (a *App) Subscribe(fn func(Notice)) (cancel func()) {
	s := &subscriber{fn: fn}
	a.ntc.mu.Lock()
	if a.ntc.subs == nil {
		a.ntc.subs = map[*subscriber]struct{}{}
	}
	a.ntc.subs[s] = struct{}{}
	a.ntc.mu.Unlock()
	return func() {
		a.ntc.mu.Lock()
		delete(a.ntc.subs, s)
		a.ntc.mu.Unlock()
	}
}

// LastUpdate is the newest update notice seen, or nil.
func (a *App) LastUpdate() *UpdateView {
	a.ntc.mu.Lock()
	defer a.ntc.mu.Unlock()
	if a.ntc.update == nil {
		return nil
	}
	u := *a.ntc.update
	u.Components = append([]string(nil), u.Components...)
	return &u
}

func (a *App) onEvent(e node.Event) {
	n, ok := noticeOf(e)
	if !ok {
		return
	}
	a.ntc.mu.Lock()
	if n.Update != nil {
		u := *n.Update
		a.ntc.update = &u
	}
	subs := make([]*subscriber, 0, len(a.ntc.subs))
	for s := range a.ntc.subs {
		subs = append(subs, s)
	}
	a.ntc.mu.Unlock()
	for _, s := range subs {
		s.fn(n)
	}
}

func noticeOf(e node.Event) (Notice, bool) {
	switch e.Kind {
	case node.EventUpdateAvailable:
		if e.Update == nil {
			return Notice{}, false
		}
		u := &UpdateView{Version: cleanVersion(e.Update.Version), Important: e.Update.Required}
		if strings.HasPrefix(e.Update.NotesURL, "https://") && !strings.ContainsAny(e.Update.NotesURL, " \t\r\n\x00") {
			u.NotesURL = e.Update.NotesURL
		}
		for _, c := range e.Update.Components {
			u.Components = append(u.Components, c.Component)
		}
		text := fmt.Sprintf("Доступна новая версия EchoFog %s. Она не устанавливается сама: страницу с описанием можно открыть и скачать обновление вручную.", u.Version)
		if u.Important {
			text = fmt.Sprintf("Важное обновление EchoFog %s: текущая версия перестаёт поддерживаться сетью, и соединения с узлами новых версий будут отклоняться. Обновление не устанавливается само: его нужно скачать вручную.", u.Version)
		}
		return Notice{Kind: NoticeUpdate, Text: text, Update: u}, true
	case node.EventNoDomesticEntry:
		err := e.Err
		if !errors.Is(err, node.ErrRouteUnsupported) {
			err = node.ErrNoDomesticEntry
		}
		return Notice{Kind: NoticeNoDomesticEntry, Text: UserMessage(err)}, true
	case node.EventReleaseUnavailable:
		return Notice{Kind: NoticeReleaseUnavailable, Text: "Данные об обновлениях повреждены, поэтому проверка обновлений отключена. Остальные функции работают."}, true
	case node.EventNoCover:
		return Notice{Kind: NoticeNoCover, Text: "У этого узла нет обложки: публичной карточки нет, и подключиться к нему можно только по приглашению."}, true
	case eventClockSkew:
		return Notice{Kind: NoticeClockSkew, Text: "Часы устройства отличаются от часов сети больше чем на 10 минут, поэтому соединение не установлено. Нужно исправить дату и время в настройках системы."}, true
	}
	return Notice{}, false
}

// cleanVersion keeps what a version number is made of: the text is shown to the user and the
// manifest is signed by the owner, but a hostile string must not reach the UI as markup.
func cleanVersion(v string) string {
	var b strings.Builder
	for _, r := range v {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-', r == '+', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}
