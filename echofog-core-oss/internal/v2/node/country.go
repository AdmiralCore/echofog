package node

// CountrySource says where the client's own country came from (T6.4c).
type CountrySource string

const (
	CountryUnknown CountrySource = "unknown" // not decided: the unknown row of the policy applies
	CountryAuto    CountrySource = "auto"    // GeoIP of a confirmed or local public address
	CountrySet     CountrySource = "set"     // the user's setting
	CountryHint    CountrySource = "hint"    // a platform hint (Android networkCountryIso)
)

// SetCountry sets the user's country; "" means automatic.
func (r *Runtime) SetCountry(cc string) {}

// SetCountryHint sets the platform's hint of the country; "" clears it.
func (r *Runtime) SetCountryHint(cc string) {}

// Country returns the client's own country and where it came from.
func (r *Runtime) OwnCountry() (string, CountrySource) { return "", CountryUnknown }
