package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The operator API answers only with the token; the local endpoint changes nothing without the
// X-Echofog header.
func TestOperatorAPIGuards(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }
	run := func(h http.HandlerFunc, method, auth, local string) int {
		r := httptest.NewRequest(method, "/api/v1/passes", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if local != "" {
			r.Header.Set(inviteHeader, local)
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	guarded := bearer("0123456789abcdef-token")(ok)
	assert.Equal(t, http.StatusUnauthorized, run(guarded, "GET", "", ""))
	assert.Equal(t, http.StatusUnauthorized, run(guarded, "GET", "Bearer wrong", ""))
	assert.Equal(t, http.StatusUnauthorized, run(guarded, "GET", "0123456789abcdef-token", ""), "the Bearer word is required")
	assert.Equal(t, http.StatusTeapot, run(guarded, "POST", "Bearer 0123456789abcdef-token", ""))

	local := localGuard(ok)
	assert.Equal(t, http.StatusTeapot, run(local, "GET", "", ""), "reading is free on loopback")
	assert.Equal(t, http.StatusForbidden, run(local, "POST", "", ""))
	assert.Equal(t, http.StatusTeapot, run(local, "POST", "", "1"))

	err := serveOperatorAPI(context.Background(), "127.0.0.1:0", "short", nil)
	require.Error(t, err, "a short token is refused")
}
