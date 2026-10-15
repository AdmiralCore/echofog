package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowedStatusHosts(t *testing.T) {
	allowed := allowedStatusHosts("127.0.0.1:7070")
	for _, h := range []string{"127.0.0.1:7070", "localhost:7070", "[::1]:7070"} {
		if !allowed[h] {
			t.Errorf("loopback host %q should be allowed", h)
		}
	}
	// A LAN bind address is accepted together with loopback, so intentional monitoring still works.
	lan := allowedStatusHosts("192.168.1.5:7070")
	if !lan["192.168.1.5:7070"] {
		t.Error("the configured bind host should be allowed")
	}
	if !lan["127.0.0.1:7070"] {
		t.Error("loopback should stay allowed even on a LAN bind")
	}
}

func TestHostGuardBlocksRebinding(t *testing.T) {
	allowed := allowedStatusHosts("127.0.0.1:7070")
	h := hostGuard(allowed, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	}))

	tests := []struct {
		name string
		host string
		want int
	}{
		{"loopback ip", "127.0.0.1:7070", http.StatusOK},
		{"localhost", "localhost:7070", http.StatusOK},
		{"ipv6 loopback", "[::1]:7070", http.StatusOK},
		{"rebinding domain", "evil.example.com:7070", http.StatusForbidden},
		{"wrong port", "127.0.0.1:9999", http.StatusForbidden},
		{"empty host", "", http.StatusForbidden},
		{"loopback no port", "127.0.0.1", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7070/status", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("Host %q: got status %d, want %d", tt.host, rec.Code, tt.want)
			}
			if tt.want != http.StatusOK && rec.Body.String() == "secret" {
				t.Errorf("Host %q: handler body leaked despite guard", tt.host)
			}
		})
	}
}
