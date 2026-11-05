package selfcover

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

type respRec struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (r *respRec) Header() http.Header         { return r.h }
func (r *respRec) WriteHeader(c int)           { r.code = c }
func (r *respRec) Write(b []byte) (int, error) { return r.body.Write(b) }

func mustReq(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "http://site.test"+path, nil)
}

// newAppServer starts a stand-in for the operator's local application.
func newAppServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "app says %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
