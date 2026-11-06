package node

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/stretchr/testify/require"
)

// noticeApp is a v2App whose notices the test fires by hand.
type noticeApp struct {
	stubV2App
	mu     sync.Mutex
	subs   []func(app.Notice)
	update *app.UpdateView
}

func (s *noticeApp) Subscribe(fn func(app.Notice)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
	return func() {}
}
func (s *noticeApp) LastUpdate() *app.UpdateView { return s.update }
func (s *noticeApp) fire(n app.Notice) {
	s.mu.Lock()
	subs := append([]func(app.Notice){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(n)
	}
}

func TestV2UpdateComesFromTheApp(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	require.Nil(t, api.V2Update(), "nothing attached")
	st := &noticeApp{update: &app.UpdateView{Version: "0.3.1", NotesURL: "https://example.org/n", Important: true, Components: []string{"proto"}}}
	api.AttachV2(st)
	require.Equal(t, &UpdateNotice{Version: "0.3.1", NotesURL: "https://example.org/n", Important: true, Components: []string{"proto"}}, api.V2Update())
	st.update = nil
	require.Nil(t, api.V2Update())

	v1 := newFacadeNode(t, config.ProtocolV1)
	v1.AttachV2(&noticeApp{update: &app.UpdateView{Version: "9"}})
	require.Nil(t, v1.V2Update(), "protocol 1 shows no v2 manifest")
}

func TestV2NoticesReachTheHandlerSetBeforeOrAfterAttach(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("handler before attach=%v", before), func(t *testing.T) {
			api := newFacadeNode(t, config.ProtocolV2)
			st := &noticeApp{}
			type got struct {
				kind, text string
				up         *UpdateNotice
			}
			var mu sync.Mutex
			var all []got
			h := func(kind, text string, up *UpdateNotice) {
				mu.Lock()
				defer mu.Unlock()
				all = append(all, got{kind, text, up})
			}
			if before {
				api.OnV2Notice(h)
				api.AttachV2(st)
			} else {
				api.AttachV2(st)
				api.OnV2Notice(h)
			}
			st.fire(app.Notice{Kind: app.NoticeUpdate, Text: "Доступна версия 0.3.1", Update: &app.UpdateView{Version: "0.3.1"}})
			st.fire(app.Notice{Kind: app.NoticeClockSkew, Text: "Часы устройства отличаются"})
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, all, 2)
			require.Equal(t, "update", all[0].kind)
			require.Equal(t, "0.3.1", all[0].up.Version)
			require.Equal(t, "clock-skew", all[1].kind)
			require.Nil(t, all[1].up)
		})
	}
}

func TestErrorScreenOfFacadeErrors(t *testing.T) {
	api := newFacadeNode(t, config.ProtocolV2)
	for err, code := range map[error]string{
		links.ErrOtherVersion:                          "other-version",
		&exit.Refusal{Reason: exit.ReasonNoCamouflage}: "no-cover",
		app.ErrFreeQuotaUsed:                           "free-quota-used",
	} {
		api.AttachV2(&stubV2App{joinErr: err})
		_, got := api.JoinInvite("x")
		require.Error(t, got)
		s, ok := ErrorScreenOf(got)
		require.True(t, ok, code)
		require.Equal(t, code, s.Code)
		require.NotEmpty(t, s.Title)
		require.NotEmpty(t, s.Text)
		require.Regexp(t, "[а-я]", got.Error())
	}
	api.AttachV2(&stubV2App{joinErr: errors.New("boom")})
	_, got := api.JoinInvite("x")
	_, ok := ErrorScreenOf(got)
	require.False(t, ok, "an ordinary error has no screen")
	_, ok = ErrorScreenOf(nil)
	require.False(t, ok)
	_, ok = ErrorScreenOf(errors.New("plain"))
	require.False(t, ok)
}
