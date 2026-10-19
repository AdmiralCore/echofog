package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeoParsersReadBothServices(t *testing.T) {
	e, err := parseIPWhoIs([]byte(`{"success":true,"country_code":"nl","country":"Netherlands","city":"Groningen"}`))
	require.NoError(t, err)
	assert.Equal(t, geoEntry{CountryCode: "NL", City: "Groningen"}, e)
	_, err = parseIPWhoIs([]byte(`{"success":false,"message":"Invalid IP address"}`))
	assert.Error(t, err)

	e, err = parseIPInfo([]byte(`{"ip":"34.158.189.116","city":"Groningen","country":"NL"}`))
	require.NoError(t, err)
	assert.Equal(t, "NL", e.CountryCode)
	_, err = parseIPInfo([]byte(`{"ip":"10.0.0.1","bogon":true}`))
	assert.Error(t, err)
}

// The first service failing hands over to the next; nobody answering is an error, not a guess.
func TestGeoLookupFallsBack(t *testing.T) {
	var firstHits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/203.0.113.9/json", r.URL.Path)
		w.Write([]byte(`{"country":"de","city":"Frankfurt am Main"}`))
	}))
	defer up.Close()

	g := &geoLocator{inFlight: map[string]bool{}, client: func() *http.Client { return http.DefaultClient },
		services: []geoService{{url: down.URL + "/%s", parse: parseIPWhoIs}, {url: up.URL + "/%s/json", parse: parseIPInfo}}}
	e, err := g.lookup(context.Background(), "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, "DE", e.CountryCode)
	assert.Equal(t, int32(1), firstHits.Load())

	g.services = g.services[:1]
	_, err = g.lookup(context.Background(), "203.0.113.9")
	assert.Error(t, err)
}

func TestGeoCacheAndAddresses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := map[string]geoEntry{
		"1.1.1.1": {CountryCode: "AU", At: now.Add(-time.Hour).Unix()},
		"2.2.2.2": {CountryCode: "FR", At: now.Add(-geoKeep - time.Hour).Unix()},
		"3.3.3.3": {Failed: true, At: now.Add(-time.Hour).Unix()},
	}
	e, due := cachedGeo(m, "1.1.1.1", now)
	assert.Equal(t, "AU", e.CountryCode)
	assert.False(t, due, "fresh answers are not looked up again")
	_, due = cachedGeo(m, "2.2.2.2", now)
	assert.True(t, due, "old answers are refreshed")
	_, due = cachedGeo(m, "3.3.3.3", now)
	assert.False(t, due, "a failed lookup waits before it is retried")
	_, due = cachedGeo(m, "4.4.4.4", now)
	assert.True(t, due)

	assert.Equal(t, "34.158.189.116", geoIP("34.158.189.116:443"))
	for _, private := range []string{"10.164.0.3:443", "192.168.1.5:4242", "127.0.0.1:1080", "100.64.1.1:443", "[fe80::1]:443"} {
		assert.Empty(t, geoIP(private), private)
	}
}

// Saved exits show a cached location, and the ones without one are looked up in the background.
func TestSavedExitsCarryLocation(t *testing.T) {
	api := historyAPI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"success":true,"country_code":"NL","city":"Groningen"}`))
	}))
	defer srv.Close()
	api.geoOnce.Do(func() {}) // install a locator that talks to the test server
	api.geoLoc = &geoLocator{inFlight: map[string]bool{}, client: func() *http.Client { return http.DefaultClient },
		services: []geoService{{url: srv.URL + "/%s", parse: parseIPWhoIs}}}

	require.NoError(t, saveVPNHistory(api.historyStorage(), []VPNSession{{ID: "1", PeerID: exitA, Addr: "203.0.113.7:443", StartedAt: 1, UpdatedAt: 2, EndedAt: 2}}))
	exits := api.GetSavedExits()
	require.Len(t, exits, 1)
	assert.Empty(t, exits[0].CountryCode, "not known yet: looked up in the background")

	require.Eventually(t, func() bool { return api.GetSavedExits()[0].CountryCode == "NL" }, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, "Groningen", api.GetSavedExits()[0].City)
}
