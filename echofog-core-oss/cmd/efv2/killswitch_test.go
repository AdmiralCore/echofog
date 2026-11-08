package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/platform"
)

func withFakes(t *testing.T, stand bool, rep platform.Report, err error) *int {
	t.Helper()
	oldRemove, oldStand := killswitchRemove, killswitchStands
	calls := 0
	killswitchRemove = func(context.Context) (platform.Report, error) { calls++; return rep, err }
	killswitchStands = func(context.Context) bool { return stand }
	t.Cleanup(func() { killswitchRemove, killswitchStands = oldRemove, oldStand })
	return &calls
}

func TestKillswitchRemove(t *testing.T) {
	calls := withFakes(t, true, platform.Report{Filters: 42}, nil)
	var out bytes.Buffer
	if err := cmdKillswitch([]string{"remove"}, &out); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || !strings.Contains(out.String(), "42") {
		t.Fatalf("calls %d, output %q", *calls, out.String())
	}
}

func TestKillswitchRemoveOnACleanComputer(t *testing.T) {
	withFakes(t, false, platform.Report{}, nil)
	var out bytes.Buffer
	if err := cmdKillswitch([]string{"remove"}, &out); err != nil {
		t.Fatalf("a clean computer is not an error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "nothing") {
		t.Fatalf("output %q", out.String())
	}
}

func TestKillswitchRemoveWithoutRightsSaysHow(t *testing.T) {
	withFakes(t, true, platform.Report{}, tun.ErrNoPrivilege)
	var out bytes.Buffer
	err := cmdKillswitch([]string{"remove"}, &out)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("error %v: it must say the terminal has to be elevated", err)
	}
}

func TestKillswitchRemoveReportsAFailure(t *testing.T) {
	withFakes(t, true, platform.Report{Filters: 3}, errors.New("bfe busy"))
	var out bytes.Buffer
	err := cmdKillswitch([]string{"remove"}, &out)
	if err == nil || !strings.Contains(err.Error(), "bfe busy") {
		t.Fatalf("error %v", err)
	}
}

func TestKillswitchStatusAndBadArguments(t *testing.T) {
	withFakes(t, true, platform.Report{}, nil)
	var out bytes.Buffer
	if err := cmdKillswitch([]string{"status"}, &out); err != nil || !strings.Contains(out.String(), "stand") {
		t.Fatalf("status: %v %q", err, out.String())
	}
	for _, bad := range [][]string{nil, {"nope"}, {"remove", "extra"}} {
		if err := cmdKillswitch(bad, &out); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := run([]string{"killswitch", "status"}); err != nil {
		t.Fatalf("run: %v", err)
	}
}
