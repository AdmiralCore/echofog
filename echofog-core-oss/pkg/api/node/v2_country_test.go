package node

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The country is a profile setting: it survives a restart, outranks GeoIP and has an explicit
// "do not specify" choice (V2-FIELD-1).
func TestV2CountrySettingPersistsAndApplies(t *testing.T) {
	p := newV2Peer(t, "country")
	p.start()

	st, err := p.api.GetCountry()
	require.NoError(t, err)
	require.Equal(t, 2, st.Protocol)
	require.Empty(t, st.Code, "a fresh profile is automatic")

	require.NoError(t, p.api.SetCountry("de"))
	st, err = p.api.GetCountry()
	require.NoError(t, err)
	require.Equal(t, "DE", st.Code, "a code is saved in upper case")
	require.Equal(t, "set", st.Source)
	require.Equal(t, "DE", st.Detected)

	set, err := loadV2Settings(p.api.v2Dir())
	require.NoError(t, err)
	require.Equal(t, "DE", set.Country, "the choice is in the profile, not only in memory")

	require.NoError(t, p.api.restartV2())
	st, err = p.api.GetCountry()
	require.NoError(t, err)
	require.Equal(t, "DE", st.Code)
	require.Equal(t, "set", st.Source)
	require.Equal(t, "DE", st.Detected, "a restart applies the saved country")

	require.NoError(t, p.api.SetCountry("none"))
	st, err = p.api.GetCountry()
	require.NoError(t, err)
	require.Equal(t, "none", st.Code)
	require.Equal(t, "unknown", st.Source)
	require.Empty(t, st.Detected, "do not specify: the unknown row, whatever GeoIP says")

	require.NoError(t, p.api.SetCountry(""))
	st, err = p.api.GetCountry()
	require.NoError(t, err)
	require.Empty(t, st.Code)
	require.NotEqual(t, "set", st.Source)

	require.Error(t, p.api.SetCountry("Deutschland"))
	set, err = loadV2Settings(p.api.v2Dir())
	require.NoError(t, err)
	require.Empty(t, set.Country, "an invalid choice does not change the profile")
}

// The desktop used to build the call layer before Runtime.Start registered call/2, so the
// transport was nil and every call was off for the whole session (V2-FIELD-1).
func TestV2StartCreatesTheCallLayer(t *testing.T) {
	p := newV2Peer(t, "callstart")
	p.start()

	p.api.v2l.Lock()
	s := p.api.v2s
	p.api.v2l.Unlock()
	require.NotNil(t, s)
	require.NotNil(t, s.rt.CallTransport(), "the runtime registered no call transport")
	require.NotNil(t, s.calls, "the desktop started without its call layer")
	require.IsType(t, &v2CallBackend{}, p.api.callDomain())
}
