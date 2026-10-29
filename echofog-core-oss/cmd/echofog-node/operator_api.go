package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
	api "github.com/echofog/echofog-core/pkg/api/node"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// The operator API: what a node owner's own Telegram bot, website or script uses to sell or hand
// out access their own way. It manages passes (personal invitation links with a term, devices and
// an optional traffic allowance) of a hidden or isolated node. See docs/operator-api.md.
//
//	GET    /api/v1/node                          mode, the shared link, how many passes
//	GET    /api/v1/passes                        every pass
//	POST   /api/v1/passes                        make one: {"label","days","devices","gb"}
//	GET    /api/v1/passes/{id}                   one pass
//	POST   /api/v1/passes/{id}/extend            add days or GB: {"days","gb","devices","label","resetUsage"}
//	POST   /api/v1/passes/{id}/revoke            stop it and drop its devices
//	DELETE /api/v1/passes/{id}                   forget it
//	DELETE /api/v1/passes/{id}/devices/{peer}    free a device's place
//
// It listens on its own address only when a token is set (ECHOFOG_API_ADDR, ECHOFOG_API_TOKEN), and
// every request carries "Authorization: Bearer <token>". The same paths answer on the local status
// endpoint for "echofog-node pass", where a change needs the X-Echofog header instead.

// minAPIToken is the shortest token the API accepts.
const minAPIToken = 16

func operatorRoutes(mux *http.ServeMux, h *api.NodeApi, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/v1/node", guard(func(w http.ResponseWriter, r *http.Request) {
		st, err := h.GetAccessStatus()
		if err != nil {
			apiError(w, err)
			return
		}
		passes, _ := h.ListPasses()
		writeJSON(w, http.StatusOK, map[string]any{"mode": st.Mode, "link": st.Link, "webLink": access.WebLink(st.Link), "joined": len(st.Joined), "passes": len(passes)})
	}))
	mux.HandleFunc("GET /api/v1/passes", guard(func(w http.ResponseWriter, r *http.Request) {
		list, err := h.ListPasses()
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.HandleFunc("POST /api/v1/passes", guard(func(w http.ResponseWriter, r *http.Request) {
		var req api.PassRequest
		if !readJSON(w, r, &req) {
			return
		}
		p, err := h.CreatePass(req)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, p)
	}))
	mux.HandleFunc("GET /api/v1/passes/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.GetPass(r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("POST /api/v1/passes/{id}/extend", guard(func(w http.ResponseWriter, r *http.Request) {
		var req api.PassRequest
		if !readJSON(w, r, &req) {
			return
		}
		p, err := h.ExtendPass(r.PathValue("id"), req)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("POST /api/v1/passes/{id}/revoke", guard(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.RevokePass(r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("DELETE /api/v1/passes/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		if err := h.DeletePass(r.PathValue("id")); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /api/v1/passes/{id}/devices/{peer}", guard(func(w http.ResponseWriter, r *http.Request) {
		if err := h.RemovePassDevice(r.PathValue("id"), r.PathValue("peer")); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// bearer lets in requests that carry the token.
func bearer(token string) func(http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + token)
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong token"})
				return
			}
			next(w, r)
		}
	}
}

// localGuard is the status endpoint's rule: reading is free on loopback, a change needs the
// X-Echofog header (which a web page in a local browser cannot send without a preflight).
func localGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get(inviteHeader) != "1" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing " + inviteHeader + " header"})
			return
		}
		next(w, r)
	}
}

// serveOperatorAPI starts the API on addr, guarded by token.
func serveOperatorAPI(ctx context.Context, addr, token string, h *api.NodeApi) error {
	if len(token) < minAPIToken {
		return fmt.Errorf("the API needs a token of at least %d characters (ECHOFOG_API_TOKEN)", minAPIToken)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	operatorRoutes(mux, h, bearer(token))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err == nil && len(strings.TrimSpace(string(body))) > 0 {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON: " + err.Error()})
		return false
	}
	return true
}

func apiError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, access.ErrNoPass):
		code = http.StatusNotFound
	case errors.Is(err, coreNode.ErrPassesNeedHidden):
		code = http.StatusConflict
	case errors.Is(err, access.ErrTooManyPass):
		code = http.StatusTooManyRequests
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
