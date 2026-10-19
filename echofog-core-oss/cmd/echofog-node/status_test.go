package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestStatsLineShowsRefusedStreams(t *testing.T) {
	line := statsLine(Status{Peers: 3, Exit: &ExitStatus{
		Enabled: true, Sessions: 5, Refused: 4,
		RefusedBy: map[string]uint64{"payment required": 3, "dial failed": 1},
	}})
	if !strings.Contains(line, "exit: 5 sessions, 4 refused (payment required 3, dial failed 1)") {
		t.Fatalf("refusals missing from %q", line)
	}
	off := statsLine(Status{Exit: &ExitStatus{Refused: 2, RefusedBy: map[string]uint64{"exit disabled": 2}}})
	if !strings.Contains(off, "exit: not sharing, 0 sessions, 2 refused (exit disabled 2)") {
		t.Fatalf("an exit that is off must say so: %q", off)
	}
}

func TestStatsLineShowsVouchersAndWhyTheyWait(t *testing.T) {
	line := statsLine(Status{Exit: &ExitStatus{Enabled: true, OwedTON: "0.74", CashedTON: "0",
		ClaimProblem: "the node wallet has no TON to pay the fee for cashing vouchers"}})
	if !strings.Contains(line, "vouchers: 0.74 TON owed, 0 TON cashed (the node wallet has no TON") {
		t.Fatalf("vouchers missing from %q", line)
	}
	quiet := statsLine(Status{Exit: &ExitStatus{Enabled: true, OwedTON: "0", CashedTON: "0"}})
	if strings.Contains(quiet, "vouchers") {
		t.Fatalf("no deposit payments: no voucher part, got %q", quiet)
	}
}
