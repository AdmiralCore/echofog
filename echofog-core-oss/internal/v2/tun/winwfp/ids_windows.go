//go:build windows

// Package winwfp is the Windows firewall driver of the kill switch (T10.6b): the rules of package
// tun as persistent Windows Filtering Platform filters in the Base Filtering Engine, under a
// provider and a sublayer of EchoFog's own. Persistent filters are kept by the BFE, not by the
// process that installed them, so a killed helper or application leaves the computer closed.
//
// Nothing here may be run with administrator rights on a shared machine: the filters cut the
// network of every session. The integration test needs the build tag wfpvm and a virtual machine.
package winwfp

import (
	"github.com/tailscale/wf"
	"golang.org/x/sys/windows"
)

// Identifiers. They are fixed: a manual removal (efv2 killswitch remove, or the PowerShell script
// of docs/deploy-vps-v2) finds the filters by the provider key, and netsh wfp show filters shows
// them by these values. Never change them: filters of an older version would be orphaned.
var (
	// ProviderKey owns every object of the kill switch.
	ProviderKey = wf.ProviderID(windows.GUID{Data1: 0xd2c88793, Data2: 0x6bad, Data3: 0x4fdf,
		Data4: [8]byte{0x88, 0x1b, 0xf5, 0x15, 0xc7, 0x96, 0xbd, 0xd7}})
	// SublayerKey is the one sublayer the filters live in.
	SublayerKey = wf.SublayerID(windows.GUID{Data1: 0xaf629ec5, Data2: 0x2cd7, Data3: 0x4717,
		Data4: [8]byte{0xaf, 0x0e, 0xb4, 0xe3, 0xe4, 0x67, 0x2d, 0x24}})
)

// Strings of the identifiers, as netsh wfp and the documentation print them.
const (
	ProviderKeyString = "{d2c88793-6bad-4fdf-881b-f515c796bdd7}"
	SublayerKeyString = "{af629ec5-2cd7-4717-af0e-b4e3e4672d24}"
)

// SublayerWeight puts the sublayer first: 0xFFFF is the highest a sublayer can have, so the Windows
// Firewall and other products are consulted after the kill switch, and a Block of ours stands
// against any Permit of theirs that does not carry the clear-action-right flag.
const SublayerWeight uint16 = 0xFFFF

// Display strings of the objects (shown by wf.msc and netsh wfp).
const (
	providerName = "EchoFog kill switch"
	providerDesc = "Keeps the computer closed outside the EchoFog tunnel. Remove: efv2 killswitch remove."
	sublayerName = "EchoFog kill switch rules"
)
