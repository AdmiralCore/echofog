package tunmode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const DeviceName = "echofog0"

func deviceURL() string { return "tun://" + DeviceName }

func preflight() error {
	if os.Geteuid() != 0 {
		return errors.New("the helper must run as root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return errors.New("the ip command (iproute2) is required")
	}
	return nil
}

// Policy-routing layout, the same idea as wg-quick:
//
//	pref 8999  UDP from the node's port       -> main (peer traffic stays on the real link)
//	pref 9000  main without its default route -> LAN and local subnets keep working
//	pref 9001  everything else                -> table 4079: default dev echofog0
const (
	table      = "4079"
	prefBypass = "8999"
	prefMain   = "9000"
	prefTun    = "9001"
)

func ipCmd(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func delRules() {
	for _, fam := range []string{"-4", "-6"} {
		for _, pref := range []string{prefBypass, prefMain, prefTun} {
			for i := 0; i < 8 && ipCmd(fam, "rule", "del", "pref", pref) == nil; i++ {
			}
		}
	}
}

func configure(bypassUDPPort int) (func(), error) {
	if _, err := waitInterface(DeviceName); err != nil {
		return nil, err
	}
	delRules() // left over from a crash
	teardown := func() {
		delRules()
		if _, err := exec.LookPath("resolvectl"); err == nil {
			_ = exec.Command("resolvectl", "revert", DeviceName).Run()
		}
	}
	if err := ipCmd("addr", "replace", tunCIDR4, "dev", DeviceName); err != nil {
		return teardown, err
	}
	if err := ipCmd("link", "set", DeviceName, "up"); err != nil {
		return teardown, err
	}
	if err := ipCmd("-4", "route", "replace", "default", "dev", DeviceName, "table", table); err != nil {
		return teardown, err
	}
	v6 := ipCmd("-6", "addr", "replace", tunCIDR6, "dev", DeviceName) == nil &&
		ipCmd("-6", "route", "replace", "default", "dev", DeviceName, "table", table) == nil

	fams := []string{"-4"}
	if v6 {
		fams = append(fams, "-6")
	}
	for _, fam := range fams {
		if bypassUDPPort > 0 {
			if err := ipCmd(fam, "rule", "add", "ipproto", "udp", "sport", strconv.Itoa(bypassUDPPort), "lookup", "main", "pref", prefBypass); err != nil {
				return teardown, fmt.Errorf("kernel 4.17+ is needed to keep the node's own traffic off the TUN: %w", err)
			}
		}
		if err := ipCmd(fam, "rule", "add", "lookup", "main", "suppress_prefixlength", "0", "pref", prefMain); err != nil {
			return teardown, err
		}
		if err := ipCmd(fam, "rule", "add", "lookup", table, "pref", prefTun); err != nil {
			return teardown, err
		}
	}
	// systemd-resolved: send every lookup to the fake DNS on the TUN. Without it, queries to a
	// LAN resolver bypass the tunnel (still answered, just not hidden from the ISP).
	if _, err := exec.LookPath("resolvectl"); err == nil {
		_ = exec.Command("resolvectl", "dns", DeviceName, tunDNS4).Run()
		_ = exec.Command("resolvectl", "domain", DeviceName, "~.").Run()
		_ = exec.Command("resolvectl", "default-route", DeviceName, "true").Run()
	}
	return teardown, nil
}

func flushDNS() {
	if _, err := exec.LookPath("resolvectl"); err == nil {
		_ = exec.Command("resolvectl", "flush-caches").Run()
	}
}

// launchElevated runs the helper as root through polkit (or directly when already root).
func launchElevated(exe string, args []string) error {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command(exe, args...)
	} else {
		if _, err := exec.LookPath("pkexec"); err != nil {
			return errors.New("pkexec (polkit) is needed to ask for administrator rights")
		}
		cmd = exec.Command("pkexec", append([]string{exe}, args...)...)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the helper: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reap; lifetime is tracked by the control connection
	return nil
}

func DefaultInterfaceIndex() (int, error) { return 0, nil }
