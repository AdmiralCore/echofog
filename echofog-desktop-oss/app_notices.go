package main

import (
	"context"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// Notices of the protocol 2 runtime and its error screens reach the UI as two events:
//
//	app:notice        { kind, text, update? }   a thing to say: a release, no entry node, a wrong clock
//	app:error-screen  { code, title, text }     a full-screen error for a failed action
//
// The texts are Russian in a neutral form; the UI picks its own words by kind and code.

// noticePayload is the body of the "app:notice" event.
func noticePayload(kind, text string, update *api.UpdateNotice) map[string]any {
	p := map[string]any{"kind": kind, "text": text}
	if update != nil {
		p["update"] = updateInfoFromNotice(version, update)
	}
	return p
}

// wireNotices passes the runtime's notices to the UI. Safe to call for any protocol: at protocol 1
// no notice ever comes.
func (a *App) wireNotices(ctx context.Context, nodeAPI *api.NodeApi) {
	nodeAPI.OnV2Notice(func(kind, text string, update *api.UpdateNotice) {
		safeEventsEmit(ctx, "app:notice", noticePayload(kind, text, update))
	})
}

// emitErrorScreen tells the UI to show a full-screen error when err has one.
func (a *App) emitErrorScreen(err error) {
	if err == nil || a.ctx == nil {
		return
	}
	if s, ok := api.ErrorScreenOf(err); ok {
		safeEventsEmit(a.ctx, "app:error-screen", s)
	}
}
