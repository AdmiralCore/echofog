package node

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/stretchr/testify/require"
)

// fakeWholeVPN is a fakeVPN of an app that has the whole-computer mode.
type fakeWholeVPN struct {
	*fakeVPN
	wmu       sync.Mutex
	supported bool
	state     tun.State
	err       string
	startErr  error
	log       []string
}

func (f *fakeWholeVPN) note(s string) { f.wmu.Lock(); f.log = append(f.log, s); f.wmu.Unlock() }

func (f *fakeWholeVPN) WholeComputerSupported() bool { return f.supported }
func (f *fakeWholeVPN) WholeComputerStart(context.Context) error {
	f.note("whole-start")
	if f.startErr != nil {
		return f.startErr
	}
	f.wmu.Lock()
	f.state = tun.StateActive
	f.wmu.Unlock()
	return nil
}
func (f *fakeWholeVPN) WholeComputerStop(context.Context) error {
	f.note("whole-stop")
	f.wmu.Lock()
	f.state = tun.StateOff
	f.wmu.Unlock()
	return nil
}
func (f *fakeWholeVPN) WholeComputerStatus() app.WholeStatus {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	return app.WholeStatus{State: f.state, Err: f.err}
}
func (f *fakeWholeVPN) WholeComputerRecover(context.Context) error {
	f.note("whole-recover")
	return nil
}
func (f *fakeWholeVPN) VPNStopForSwitch() error { f.note("stop-for-switch"); return nil }

func newWholeApi(t *testing.T, supported bool) (*NodeApi, *fakeWholeVPN) {
	t.Helper()
	api := newFacadeNode(t, config.ProtocolV2)
	f := &fakeWholeVPN{fakeVPN: &fakeVPN{}, supported: supported}
	api.AttachV2(&fakeV2App{})
	api.AttachV2VPN(f)
	return api, f
}

func TestV2WholeComputerFollowsTheApp(t *testing.T) {
	api, f := newWholeApi(t, true)
	require.True(t, api.WholeComputerSupported())
	require.False(t, api.KillSwitchOn(), "off while the mode is off")
	require.NoError(t, api.SetWholeComputer(true))
	require.True(t, api.KillSwitchOn(), "the kill switch is part of the mode")
	st := api.GetVPNStatus()
	require.True(t, st.WholeComputerSupported)
	require.True(t, st.WholeComputer)
	require.True(t, st.KillSwitch)
	require.False(t, st.Blocked)
	require.NoError(t, api.SetWholeComputer(false))
	require.Equal(t, []string{"whole-start", "whole-stop"}, f.log)
	require.False(t, api.GetVPNStatus().WholeComputer)
}

func TestV2HeldIsShownAsBlockedEvenWithTheVPNStopped(t *testing.T) {
	api, f := newWholeApi(t, true)
	f.state, f.err = tun.StateHeld, "the tunnel was lost"
	st := api.GetVPNStatus()
	require.False(t, st.Running, "the VPN itself is not running")
	require.True(t, st.Blocked, "Held blocks the whole computer: the screen must say so")
	require.True(t, st.WholeComputerSupported)
	require.Contains(t, st.WholeComputerError, "потерян", "a neutral Russian text, not the internal sentence")
	require.False(t, strings.Contains(st.WholeComputerError, "tun:"))
}

func TestV2KillSwitchCannotBeSwitchedOffInsideTheMode(t *testing.T) {
	api, _ := newWholeApi(t, true)
	require.NoError(t, api.SetKillSwitch(true))
	err := api.SetKillSwitch(false)
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "app:"))
}

func TestV2SwitchOfExitKeepsTheMode(t *testing.T) {
	api, f := newWholeApi(t, true)
	require.NoError(t, api.StopVPNForSwitch())
	require.Equal(t, []string{"stop-for-switch"}, f.log)
	require.False(t, f.called("stop"), "a switch is not a stop of the VPN")
}

func TestV2WholeComputerErrorsAreNeutralRussian(t *testing.T) {
	api, f := newWholeApi(t, true)
	for _, err := range []error{app.ErrNeedVPN, tun.ErrNoPrivilege, tun.ErrUnsupported, app.ErrWholeOff} {
		f.startErr = err
		got := api.SetWholeComputer(true)
		require.Error(t, got)
		require.Equal(t, app.UserMessage(err), got.Error())
		require.False(t, strings.Contains(got.Error(), "app:") || strings.Contains(got.Error(), "tun:"), got.Error())
		require.NotEqual(t, app.UserMessage(errGeneric), got.Error(), "%v has its own text", err)
	}
}

func TestV2WithoutDriversTheModeIsNotThere(t *testing.T) {
	api, f := newWholeApi(t, false)
	require.False(t, api.WholeComputerSupported())
	require.ErrorIs(t, api.SetWholeComputer(true), ErrV2NotBuilt)
	require.False(t, api.GetVPNStatus().WholeComputerSupported)
	require.NoError(t, api.StopVPNForSwitch())
	require.False(t, f.called("whole-start"))
}

var errGeneric = context.DeadlineExceeded

// Filters of an earlier run are looked for as soon as the VPN backend is attached: a computer that is
// held must show it before the user touches anything.
func TestV2AttachRecoversTheHeldState(t *testing.T) {
	_, f := newWholeApi(t, true)
	require.Equal(t, []string{"whole-recover"}, f.log)
	_, g := newWholeApi(t, false)
	require.Empty(t, g.log, "an app without the mode is not asked")
}
