package node

import "strings"

// CountrySource says where the client's own country came from (T6.4c).
type CountrySource string

const (
	CountryUnknown CountrySource = "unknown" // not decided: the unknown row of the policy applies
	CountryAuto    CountrySource = "auto"    // GeoIP of a confirmed or local public address
	CountrySet     CountrySource = "set"     // the user's setting
	CountryHint    CountrySource = "hint"    // a platform hint (Android networkCountryIso)
)

// normCountry accepts a two-letter code and returns it in upper case; anything else is "".
func normCountry(cc string) string {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if len(cc) != 2 || cc[0] < 'A' || cc[0] > 'Z' || cc[1] < 'A' || cc[1] > 'Z' {
		return ""
	}
	return cc
}

// SetCountry sets the user's country; "" (or anything that is not a two-letter code) means
// automatic. It outranks every other source. New paths follow the new row; live sessions stay.
func (r *Runtime) SetCountry(cc string) {
	r.countryMu.Lock()
	r.countryUser = normCountry(cc)
	r.countryMu.Unlock()
	r.noteCountry()
}

// SetCountryHint sets the platform's hint of the country (Android networkCountryIso); "" clears
// it. It ranks below GeoIP. The time zone and the language of the system are no source at all.
func (r *Runtime) SetCountryHint(cc string) {
	r.countryMu.Lock()
	r.countryHint = normCountry(cc)
	r.countryMu.Unlock()
	r.noteCountry()
}

// OwnCountry returns the client's own country and where it came from, by priority: the user's
// setting, the GeoIP of the confirmed public address (OBSERVED_ADDR, wire.md §7.2) or of a public
// local address, the platform hint. Without any of them it is ("", CountryUnknown).
func (r *Runtime) OwnCountry() (string, CountrySource) {
	r.countryMu.Lock()
	user, hint := r.countryUser, r.countryHint
	r.countryMu.Unlock()
	if user != "" {
		return user, CountrySet
	}
	if ip, ok := r.PublicAddr(); ok {
		if c, ok := r.Country(ip.Unmap()); ok && c != "" {
			return c, CountryAuto
		}
	}
	for _, a := range r.planInfo().Addrs {
		if c, ok := r.Country(a.Unmap()); ok && c != "" {
			return c, CountryAuto
		}
	}
	if hint != "" {
		return hint, CountryHint
	}
	return "", CountryUnknown
}

// ownCountry is the country the policy decides by; "" selects the unknown row.
func (r *Runtime) ownCountry() string {
	cc, _ := r.OwnCountry()
	return cc
}

// noteCountry tells the application when the country the policy uses has changed.
func (r *Runtime) noteCountry() {
	cc, _ := r.OwnCountry()
	r.countryMu.Lock()
	changed := cc != r.countryLast
	r.countryLast = cc
	r.countryMu.Unlock()
	if !changed {
		return
	}
	msg := "country decided"
	if cc == "" {
		msg = "country is not known"
	}
	r.events.Emit(Event{Kind: EventCountryChanged, Message: msg, At: r.clock.Now()})
}
