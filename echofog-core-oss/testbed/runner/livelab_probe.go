package main

// The probe phase of the live lab (T15.0f): the active probes D11-D15 asked from the border router
// gw_b, never from a node, against the real carriers of x1.

import (
	"crypto/ecdh"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/testbed/labkeys"
	"github.com/echofog/echofog-core/testbed/probe"
	"github.com/echofog/echofog-core/testbed/probe/r2flight"
)

const (
	probeAlt    = "203.0.113.99" // the second source address of the prober: an alias on gw_b
	probeWindow = 330            // seconds: a little past the token timestamp window of reality2 (300 s)
)

// liveProbePhase starts the servers of the TCP and QUIC carriers (the visit phase is over and has
// stopped them), asks them, and stops them. Each TCP case is two runs of the prober: the first
// holds a first flight that the node admitted, replayed at once and after the window; the second
// takes flights made just before it, so their timestamps are fresh (D15).
func liveProbePhase(v *verdict, out string, seed int64) []probe.Report {
	const gw = "gw_b"
	c := container(gw)
	s := strconv.FormatInt(seed, 10)
	carrierBin, err := buildCarrierBinary()
	if err != nil {
		v.check(false, "probe phase: %v", err)
		return nil
	}
	probeBin, err := buildLinuxBinary("echofog-launchpad-probe-linux", "./testbed/probe/cmd/launchpad-probe")
	if err != nil {
		v.check(false, "probe phase: %v", err)
		return nil
	}
	for _, f := range []struct{ src, container, dst string }{
		{carrierBin, c, "/tmp/v2carrier"}, {probeBin, c, "/tmp/launchpad-probe"}, {carrierBin, container("x1"), "/tmp/v2carrier"},
	} {
		if o, err := run("docker", "cp", f.src, f.container+":"+f.dst); err != nil {
			v.check(false, "probe phase: copy %s to %s: %v %s", f.dst, f.container, err, o)
			return nil
		}
		_, _ = run("docker", "exec", f.container, "chmod", "+x", f.dst)
	}
	// the second source address: an alias on the wan side of the border router
	wanRaw, _ := run("docker", "exec", c, "cat", "/tmp/wan_if")
	wan := strings.TrimSpace(wanRaw)
	if o, err := run("docker", "exec", c, "ip", "addr", "add", probeAlt+"/24", "dev", wan); err != nil {
		v.check(false, "probe phase: add the second source address on %s: %v %s", gw, err, o)
		return nil
	}
	defer func() { _, _ = run("docker", "exec", c, "ip", "addr", "del", probeAlt+"/24", "dev", wan) }()

	for _, lc := range liveCarriers {
		if lc.suite == "noise3" {
			continue // the prober has no noise3 first flight: noise3 is not in this phase
		}
		_, _ = run("docker", "exec", container("x1"), "rm", "-f", liveReady(lc.port))
		args := []string{"exec", "-d", container("x1"), "/tmp/v2carrier", "-mode", "real-serve", "-suite", lc.suite,
			"-listen", fmt.Sprintf(":%d", lc.port), "-advertise", fmt.Sprintf("%s:%d", xIP, lc.port),
			"-cover", webIP + ":443", "-seed", s, "-ready", liveReady(lc.port)}
		switch lc.suite {
		case "ws2":
			args = append(args, "-ca", "/tmp/ws2-cdn.pem", "-ref-listen", fmt.Sprintf(":%d", ws2RefPort))
		case "reality2-quic":
			args = append(args, "-ref-listen", fmt.Sprintf(":%d", quicRefPort))
		}
		if o, err := run("docker", args...); err != nil {
			v.check(false, "probe phase: start the %s server: %v %s", lc.suite, err, o)
			return nil
		}
	}
	defer func() { _, _ = run("docker", "exec", container("x1"), "pkill", "-f", "/tmp/v2carrier") }()
	for _, lc := range liveCarriers {
		if lc.suite == "noise3" {
			continue
		}
		ready := liveReady(lc.port)
		if !waitFor("the "+lc.suite+" server", 30*time.Second, func() bool {
			_, err := run("docker", "exec", container("x1"), "sh", "-c", "test -f "+ready)
			return err == nil
		}) {
			v.check(false, "probe phase: the %s server did not become ready", lc.suite)
			return nil
		}
	}

	var reps []probe.Report
	collect := func(name, spec string) {
		raw, err := runWithSpec(c, name, spec)
		var got []probe.Report
		if jerr := json.Unmarshal(jsonArray(raw), &got); jerr != nil {
			v.check(false, "probe phase %s: no reports: %v %v", name, err, jerr)
			return
		}
		for i := range got {
			got[i].Target = name
		}
		reps = append(reps, got...)
	}

	tokenAddr := fmt.Sprintf("%s:%d", xIP, 4351)
	pub := labkeys.Mask(seed, "reality2-tcp").PublicKey().Bytes()
	first, err := r2flight.Flight(pub, tokenAddr, "cover.lab", time.Now())
	if err != nil {
		v.check(false, "probe phase: first flight: %v", err)
		return reps
	}
	if o, err := run("docker", "exec", c, "/tmp/v2carrier", "-mode", "send-hex", "-peer", tokenAddr, "-hex", hex.EncodeToString(first)); err != nil {
		v.check(false, "probe phase: admit the first flight of reality2-tcp: %v %s", err, o)
		return reps
	}
	logf("probe: reality2-tcp replay (waits %d s past the timestamp window)", probeWindow)
	collect("reality2-tcp/replay", specJSON(map[string]any{
		"scenario": "ordinary", "network": "tcp", "addr": tokenAddr, "cover": webIP + ":443", "sni": "cover.lab",
		"first_flight": hex.EncodeToString(first), "window_seconds": probeWindow, "listen_ms": 800, "alt_local": probeAlt,
	}))
	// fresh flights for D15, made now
	other, err := ecdhPub()
	if err != nil {
		v.check(false, "probe phase: foreign key: %v", err)
		return reps
	}
	valid, err1 := r2flight.Flight(pub, tokenAddr, "cover.lab", time.Now())
	foreign, err2 := r2flight.Flight(other, tokenAddr, "cover.lab", time.Now())
	if err1 != nil || err2 != nil {
		v.check(false, "probe phase: token flights: %v %v", err1, err2)
		return reps
	}
	collect("reality2-tcp", specJSON(map[string]any{
		"scenario": "ordinary", "network": "tcp", "addr": tokenAddr, "cover": webIP + ":443", "sni": "cover.lab",
		"valid": hex.EncodeToString(valid), "foreign": hex.EncodeToString(foreign), "window_seconds": 0, "listen_ms": 800,
	}))
	// reality2-quic: the prober has no QUIC first-flight builder, so D11 and D15 are not measured;
	// D12, D13 and D14 compare the node with the twin cover
	collect("reality2-quic", specJSON(map[string]any{
		"scenario": "ordinary", "network": "udp", "addr": fmt.Sprintf("%s:%d", xIP, 4352), "cover": fmt.Sprintf("%s:%d", xIP, quicRefPort),
		"sni": "cover.lab", "window_seconds": 0, "listen_ms": 2000,
	}))
	// ws2: HTTPS questions to the front of the node and to the reference front
	if raw, err := run("docker", "exec", c, "/tmp/v2carrier", "-mode", "ws2-probe",
		"-peer", fmt.Sprintf("%s:%d", xIP, 4353), "-ref", fmt.Sprintf("%s:%d", xIP, ws2RefPort)); err != nil {
		v.check(false, "probe phase: ws2: %v %s", err, raw)
	} else {
		var got []probe.Report
		if jerr := json.Unmarshal(jsonArray(raw), &got); jerr != nil {
			v.check(false, "probe phase: ws2 reports: %v", jerr)
		} else {
			reps = append(reps, got...)
		}
	}
	_ = writeJSON(filepath.Join(out, "probes.json"), reps)
	return reps
}

func specJSON(entry map[string]any) string {
	b, _ := json.Marshal([]map[string]any{entry})
	return string(b)
}

// runWithSpec writes the spec into the container and runs the prober on it. Exit 1 only means
// that a node gave an answer of its own: the reports are on stdout all the same.
func runWithSpec(c, name, spec string) (string, error) {
	path := "/tmp/probe-" + strings.ReplaceAll(name, "/", "-") + ".json"
	tmp := filepath.Join(os.TempDir(), "probe-spec.json")
	if err := os.WriteFile(tmp, []byte(spec), 0o600); err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if o, err := run("docker", "cp", tmp, c+":"+path); err != nil {
		return o, err
	}
	return run("docker", "exec", c, "/tmp/launchpad-probe", "-spec", path, "-timeout", "20m")
}

func ecdhPub() ([]byte, error) {
	k, err := ecdh.X25519().GenerateKey(crand.Reader)
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}

// jsonArray cuts the JSON list out of the output: the container command mixes its error stream into
// it (the prober prints a line on stderr when a node answered on its own).
func jsonArray(raw string) []byte {
	i, j := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if i < 0 || j < i {
		return []byte(raw)
	}
	return []byte(raw[i : j+1])
}
