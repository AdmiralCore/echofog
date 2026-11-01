//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// The tests write the user's real Run key, under a name of their own that they remove again; the
// entry "EchoFog" is never touched.

// testExe is never started: if a test run is killed before its cleanup, sign-in finds no program.
const testExe = `C:\EchoFog test\does-not-exist\EchoFog.exe`

func testAutostartName(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("EchoFog-test-%d-%s", os.Getpid(), t.Name())
	cleanup := func() {
		_ = deleteValue(runKeyPath, name)
		_ = deleteValue(startupApprovedPath, name)
	}
	cleanup()
	t.Cleanup(cleanup)
	return name
}

func runValue(t *testing.T, name string) (string, bool) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open the Run key: %v", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	return v, err == nil
}

func approvedValue(t *testing.T, name string) bool {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, startupApprovedPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetBinaryValue(name)
	return err == nil
}

// switchInStartupApps sets the entry the way Windows' startup apps do: 12 bytes, 2 on, 3 off.
func switchInStartupApps(t *testing.T, name string, on bool) {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, startupApprovedPath, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("open StartupApproved: %v", err)
	}
	defer k.Close()
	v := make([]byte, 12)
	v[0] = 3
	if on {
		v[0] = 2
	}
	if err := k.SetBinaryValue(name, v); err != nil {
		t.Fatalf("write StartupApproved: %v", err)
	}
}

func TestAutostartRunKeyRoundTrip(t *testing.T) {
	name := testAutostartName(t)
	if st := autostartState(name, testExe); !st.Supported || st.Enabled || st.Blocked || st.Program != "" {
		t.Fatalf("before: %+v, want supported and off", st)
	}

	if err := setAutostart(name, testExe, true); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if v, ok := runValue(t, name); !ok || v != `"`+testExe+`" --autostart` {
		t.Fatalf("Run value = %q (%v), want the quoted program and %s", v, ok, autostartFlag)
	}
	if st := autostartState(name, testExe); !st.Enabled || st.Blocked || st.Program != "" {
		t.Fatalf("on: %+v, want enabled for this copy", st)
	}
	if st := autostartState(name, strings.ToUpper(testExe)); st.Program != "" {
		t.Fatalf("the same program in other letter case is reported as another copy: %q", st.Program)
	}
	if st := autostartState(name, `C:\Other\EchoFog.exe`); !st.Enabled || st.Program != testExe {
		t.Fatalf("from another copy: %+v, want enabled with program %q", st, testExe)
	}

	if err := setAutostart(name, testExe, false); err != nil {
		t.Fatalf("switch off: %v", err)
	}
	if _, ok := runValue(t, name); ok {
		t.Fatal("the Run value is still there after switching off")
	}
	if st := autostartState(name, testExe); st.Enabled {
		t.Fatalf("off: %+v", st)
	}
	if err := setAutostart(name, testExe, false); err != nil {
		t.Fatalf("switching off again: %v", err)
	}
}

func TestAutostartSwitchedOffInStartupApps(t *testing.T) {
	name := testAutostartName(t)
	if err := setAutostart(name, testExe, true); err != nil {
		t.Fatal(err)
	}
	switchInStartupApps(t, name, false)
	if st := autostartState(name, testExe); st.Enabled || !st.Blocked {
		t.Fatalf("off in startup apps: %+v, want blocked and not enabled", st)
	}
	switchInStartupApps(t, name, true)
	if st := autostartState(name, testExe); !st.Enabled || st.Blocked {
		t.Fatalf("on in startup apps: %+v, want enabled", st)
	}

	// switching it on in the app takes back the "off" of startup apps
	switchInStartupApps(t, name, false)
	if err := setAutostart(name, testExe, true); err != nil {
		t.Fatal(err)
	}
	if approvedValue(t, name) {
		t.Fatal("the startup apps entry is still there after switching on in the app")
	}
	if st := autostartState(name, testExe); !st.Enabled || st.Blocked {
		t.Fatalf("after switching on: %+v", st)
	}

	// switching off removes both
	switchInStartupApps(t, name, false)
	if err := setAutostart(name, testExe, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := runValue(t, name); ok || approvedValue(t, name) {
		t.Fatal("an entry is left after switching off")
	}
}

func TestAutostartAppMethods(t *testing.T) {
	name := testAutostartName(t)
	real := autostartName
	autostartName = name
	t.Cleanup(func() { autostartName = real })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	a := NewApp()
	if st := a.GetAutostart(); !st.Supported || st.Enabled {
		t.Fatalf("before: %+v", st)
	}
	if err := a.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	if v, _ := runValue(t, name); commandProgram(v) != exe {
		t.Fatalf("Run value %q does not start this program %q", v, exe)
	}
	if st := a.GetAutostart(); !st.Enabled || st.Program != "" {
		t.Fatalf("on: %+v, want enabled for this copy", st)
	}
	if err := a.SetAutostart(false); err != nil {
		t.Fatal(err)
	}
	if st := a.GetAutostart(); st.Enabled {
		t.Fatalf("off: %+v", st)
	}
}

func TestCommandProgram(t *testing.T) {
	for cmd, want := range map[string]string{
		`"C:\Program Files\EchoFog\EchoFog.exe" --autostart`: `C:\Program Files\EchoFog\EchoFog.exe`,
		`"C:\EchoFog.exe"`:                    `C:\EchoFog.exe`,
		`C:\Tools\EchoFog.exe --autostart`:    `C:\Tools\EchoFog.exe`,
		`  "C:\a b\EchoFog.exe"  --autostart`: `C:\a b\EchoFog.exe`,
		`"C:\unclosed`:                        `C:\unclosed`,
	} {
		if got := commandProgram(cmd); got != want {
			t.Errorf("commandProgram(%q) = %q, want %q", cmd, got, want)
		}
	}
}
