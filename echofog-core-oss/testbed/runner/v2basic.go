package main

// v2-basic: two efv2 nodes on the lab WAN, one listening on noise3, the other dialing it and
// echoing a byte run and a handful of datagrams. The scenario reports the throughput as the
// baseline (there is no pass/fail threshold for it) and checks the session the server sees.
//
// reality2-quic joins this scenario when T2.6b (the reproducible cover) is merged; the efv2 CLI
// already parses the listener form and refuses it with that reason.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/testbed/browserprofiles"
)

const (
	v2BasicServer = "v2b"
	v2BasicClient = "v2a"
	v2CardsDir    = "efv2-cards"
)

type v2EchoResult struct {
	Suite             uint16  `json:"suite"`
	Path              string  `json:"path"`
	RTTMs             float64 `json:"rtt_ms"`
	BytesSent         int64   `json:"bytes_sent"`
	BytesReceived     int64   `json:"bytes_received"`
	Seconds           float64 `json:"seconds"`
	Mbps              float64 `json:"mbps"`
	DatagramsSent     int     `json:"datagrams_sent"`
	DatagramsReceived int     `json:"datagrams_received"`
	PeerNodeID        string  `json:"peer_node_id"`
	PeerDeviceID      string  `json:"peer_device_id"`
}

type v2StatusDoc struct {
	NodeID   string `json:"node_id"`
	DeviceID string `json:"device_id"`
	Sessions []struct {
		NodeID    string   `json:"node_id"`
		DeviceID  string   `json:"device_id"`
		Suite     uint16   `json:"suite"`
		Path      string   `json:"path"`
		Paths     []uint64 `json:"paths"`
		Standby   int      `json:"standby"`
		Abuse     uint64   `json:"abuse"`
		GoingAway bool     `json:"going_away"`
	} `json:"sessions"`
	Abuse []struct {
		Source string `json:"source"`
		Count  uint64 `json:"count"`
	} `json:"abuse"`
}

type v2CardDoc struct {
	NodeID   string `json:"node_id"`
	DeviceID string `json:"device_id"`
}

// v2BasicBringUp builds the image and starts the two efv2 containers. The server writes its card
// once its runtime is up; the client container only creates its identity and waits for the echo
// command (two processes cannot open the same store at once).
func v2BasicBringUp() {
	logf("tearing down what is left of an earlier run")
	_, _ = compose(nil, "down", "-v", "--remove-orphans")
	if !*noBuild {
		logf("building the image")
		if out, err := compose(nil, "build"); err != nil {
			fatalf("build: %v\n%s", err, out)
		}
	}
	logf("starting the two efv2 nodes")
	if out, err := compose(nil, "up", "-d", v2BasicClient, v2BasicServer); err != nil {
		fatalf("up: %v\n%s", err, out)
	}
	if !waitFor(v2BasicServer+" to write its peer card", 90*time.Second, func() bool {
		_, err := run("docker", "exec", container(v2BasicServer), "test", "-f", "/data/"+v2CardsDir+"/peer.json")
		return err == nil
	}) {
		fatalf("%s did not start (docker logs %s)", v2BasicServer, container(v2BasicServer))
	}
}

func runV2Basic(v *verdict, out string) {
	seed := *seed
	if seed == 0 {
		seed = time.Now().UnixNano()
		logf("seed %d (-seed %d repeats the run)", seed, seed)
	}
	behaviour := map[string]any{
		"seed": seed, "profile": *v2Profile, "network": *v2Network,
		"echo_bytes_requested": *v2EchoBytes,
	}
	findings := []string{
		"v2-basic runs noise3 only; reality2-quic joins when T2.6b is merged",
		"the echo throughput is the baseline of this machine and is not gated",
	}

	cardText, err := runStdout("docker", "exec", container(v2BasicServer), "cat", "/data/"+v2CardsDir+"/peer.json")
	if err != nil {
		v.check(false, "read the %s peer card: %v", v2BasicServer, err)
		writeBasicReport(v, out, seed, behaviour, findings, nil, nil)
		return
	}
	cardPath := filepath.Join(out, v2BasicServer+"-peer.json")
	if err := os.WriteFile(cardPath, []byte(cardText+"\n"), 0o644); err != nil {
		v.check(false, "save the peer card: %v", err)
	}
	var card v2CardDoc
	if err := json.Unmarshal([]byte(cardText), &card); err != nil {
		v.check(false, "parse the peer card: %v", err)
		writeBasicReport(v, out, seed, behaviour, findings, nil, nil)
		return
	}
	logf("%s: node %s device %s", v2BasicServer, shortID(card.NodeID), shortID(card.DeviceID))
	behaviour["server_node_id"] = card.NodeID
	behaviour["server_device_id"] = card.DeviceID

	if cpOut, err := run("docker", "cp", cardPath, container(v2BasicClient)+":/tmp/peer.json"); err != nil {
		v.check(false, "copy the peer card into %s: %v %s", v2BasicClient, err, cpOut)
		writeBasicReport(v, out, seed, behaviour, findings, nil, nil)
		return
	}

	logf("echo: %s dials %s and sends %s", v2BasicClient, shortID(card.DeviceID), humanBytes(*v2EchoBytes))
	echoText, err := runStdout("docker", "exec", container(v2BasicClient), "efv2", "echo", card.DeviceID,
		"--data", "/data", "--card", "/tmp/peer.json",
		"--bytes", strconv.FormatInt(*v2EchoBytes, 10), "--datagrams", "100", "--timeout", "10m")
	if err != nil {
		v.check(false, "efv2 echo: %v", err)
		writeBasicReport(v, out, seed, behaviour, findings, nil, nil)
		return
	}
	var res v2EchoResult
	if err := json.Unmarshal([]byte(echoText), &res); err != nil {
		v.check(false, "parse the echo result: %v\n%s", err, echoText)
	} else {
		_ = os.WriteFile(filepath.Join(out, "echo.json"), []byte(echoText+"\n"), 0o644)
		logf("echo: suite %d path %s rtt %.2f ms, %s in %.2f s = %.1f Mbit/s, datagrams %d/%d",
			res.Suite, res.Path, res.RTTMs, humanBytes(res.BytesReceived), res.Seconds, res.Mbps,
			res.DatagramsReceived, res.DatagramsSent)
		v.check(res.BytesSent == *v2EchoBytes && res.BytesReceived == *v2EchoBytes,
			"echo stream: %s of %s returned", humanBytes(res.BytesReceived), humanBytes(*v2EchoBytes))
		v.check(res.Suite == 1, "the session uses noise3 (suite %d)", res.Suite)
		v.check(res.Path == "direct", "the session is direct (path %s)", res.Path)
		v.check(res.DatagramsSent == 100 && res.DatagramsReceived == 100,
			"datagram echo: %d of %d replies", res.DatagramsReceived, res.DatagramsSent)
		v.check(res.Mbps > 0, "echo throughput baseline %.1f Mbit/s (no threshold)", res.Mbps)
		behaviour["echo"] = res
	}

	statusText, statusErr := runStdout("docker", "exec", container(v2BasicServer), "efv2", "status", "--data", "/data")
	if statusErr != nil {
		v.check(false, "efv2 status on %s: %v", v2BasicServer, statusErr)
		writeBasicReport(v, out, seed, behaviour, findings, &res, nil)
		return
	}
	_ = os.WriteFile(filepath.Join(out, v2BasicServer+"-status.json"), []byte(statusText+"\n"), 0o644)
	var status v2StatusDoc
	if err := json.Unmarshal([]byte(statusText), &status); err != nil {
		v.check(false, "parse the status snapshot: %v", err)
		writeBasicReport(v, out, seed, behaviour, findings, &res, nil)
		return
	}
	if len(status.Sessions) >= 1 {
		s := status.Sessions[0]
		logf("status: %d session(s); %s suite %d path %s standby %d paths %v abuse %d",
			len(status.Sessions), shortID(s.DeviceID), s.Suite, s.Path, s.Standby, s.Paths, s.Abuse)
		v.check(s.Suite == 1, "the server sees a noise3 session (suite %d)", s.Suite)
		v.check(s.Path == "direct", "the server sees a direct path (%s)", s.Path)
		v.check(s.DeviceID != "" && s.NodeID != "", "the server knows the authenticated peer")
	} else {
		v.check(false, "the server status lists %d sessions, want at least 1", len(status.Sessions))
	}
	behaviour["server_sessions"] = len(status.Sessions)
	writeBasicReport(v, out, seed, behaviour, findings, &res, &status)
}

func writeBasicReport(v *verdict, out string, seed int64, behaviour map[string]any, findings []string, res *v2EchoResult, status *v2StatusDoc) {
	resources := map[string]any{}
	if status != nil {
		resources[v2BasicServer] = status
	}
	report := buildReport(reportInput{
		Scenario: *scenario, Seed: seed, Profile: *v2Profile, Network: *v2Network,
		Phases: []string{"echo"}, Behaviour: behaviour, Resources: resources,
		Carrier:         carrierInfo{Name: "noise3 (efv2)", Note: "two efv2 containers on the lab WAN, direct UDP"},
		BrowserProfiles: browserProfileStamp{Version: browserprofiles.DBVersion, Note: "not used by v2-basic"},
		Pcap:            pcapInfo{Reason: "v2-basic captures no packets"},
		Findings:        findings,
	})
	if err := writeJSON(filepath.Join(out, "report.json"), report); err != nil {
		v.check(false, "write report.json: %v", err)
	}
	if f, err := os.Create(filepath.Join(out, "flows.csv")); err == nil {
		_ = writeFlowsCSV(f, nil, 200)
		_ = f.Close()
	}
	var summary strings.Builder
	writeSummary(&summary, report, nil)
	if err := os.WriteFile(filepath.Join(out, "summary.txt"), []byte(summary.String()), 0o644); err != nil {
		v.check(false, "write summary.txt: %v", err)
	}
	if path, err := saveResult(report, filepath.Join("testbed", "results")); err != nil {
		v.check(false, "save the results copy: %v", err)
	} else {
		logf("results copy: %s", path)
	}
	logf("report: %s", filepath.Join(out, "report.json"))
}

// runStdout is `run` with stdout and stderr separated: efv2 prints its JSON to stdout and its
// diagnostics to stderr, and the scenario must parse the former.
func runStdout(args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
