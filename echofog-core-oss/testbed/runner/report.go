package main

// report.json v2: one schema for every launchpad run, and the small files around it. The numbers
// are stable and named the same way between runs, so `compare` is a diff rather than a story.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/testbed/censor"
)

// aggregate is the censor's summary over the product and carrier flows.
type aggregate struct {
	VisibilityPct       float64 `json:"visibility_pct"`
	StrictVisibilityPct float64 `json:"strict_visibility_pct"`
	CarrierFlows        int     `json:"carrier_flows"`
	CarrierBytes        int     `json:"carrier_bytes"`
	AtRiskFlows         int     `json:"at_risk_flows"`
}

type coverageRow struct {
	Threat   string `json:"threat"`
	Detector string `json:"detector"`
	Verdict  string `json:"verdict"`
	Note     string `json:"note,omitempty"`
}

type pcapInfo struct {
	Requested     bool     `json:"requested"`
	BudgetBytes   int64    `json:"budget_bytes"`
	CapturedBytes int64    `json:"captured_bytes"`
	Kept          bool     `json:"kept"`
	Deleted       bool     `json:"deleted"`
	Reason        string   `json:"reason,omitempty"`
	Files         []string `json:"files,omitempty"`
}

type browserProfileStamp struct {
	Version string   `json:"version"`
	Entries int      `json:"entries"`
	Sources []string `json:"sources,omitempty"`
	Note    string   `json:"note,omitempty"`
}

type carrierInfo struct {
	Name      string   `json:"name"`
	Synthetic bool     `json:"synthetic"`
	Note      string   `json:"note,omitempty"`
	Modes     []string `json:"modes,omitempty"`
	Bytes     int64    `json:"bytes,omitempty"`
	Packets   int64    `json:"packets,omitempty"`
}

type flowCounts struct {
	Total    int `json:"total"`
	Labelled int `json:"labelled"`
	Carrier  int `json:"carrier"`
	Product  int `json:"product"`
	Browser  int `json:"browser"`
	Tool     int `json:"tool"`
}

type reportV2 struct {
	Schema          int                              `json:"schema"`
	When            string                           `json:"when"`
	Commit          string                           `json:"commit"`
	Spec            string                           `json:"spec"`
	Profile         string                           `json:"profile"`
	Scenario        string                           `json:"scenario"`
	Seed            int64                            `json:"seed"`
	Network         string                           `json:"network"`
	Phases          []string                         `json:"phases"`
	Detectors       map[string]censor.DetectorMetric `json:"detectors"`
	Aggregate       aggregate                        `json:"aggregate"`
	Coverage        []coverageRow                    `json:"coverage"`
	Behaviour       map[string]any                   `json:"behaviour"`
	Resources       map[string]any                   `json:"resources"`
	Carrier         carrierInfo                      `json:"carrier"`
	BrowserProfiles browserProfileStamp              `json:"browser_profiles"`
	Pcap            pcapInfo                         `json:"pcap"`
	Flows           flowCounts                       `json:"flows"`
	Findings        []string                         `json:"findings"`
}

type reportInput struct {
	Scenario        string
	Seed            int64
	Profile         string
	Network         string
	Phases          []string
	Detectors       map[string]censor.DetectorMetric
	Aggregate       aggregate
	Coverage        []coverageRow
	Behaviour       map[string]any
	Resources       map[string]any
	Carrier         carrierInfo
	BrowserProfiles browserProfileStamp
	Pcap            pcapInfo
	Flows           flowCounts
	Findings        []string
}

func buildReport(in reportInput) reportV2 {
	if in.Detectors == nil {
		in.Detectors = map[string]censor.DetectorMetric{}
	}
	if in.Behaviour == nil {
		in.Behaviour = map[string]any{}
	}
	if in.Resources == nil {
		in.Resources = map[string]any{}
	}
	return reportV2{
		Schema: 2, When: time.Now().UTC().Format(time.RFC3339), Commit: commit(), Spec: specVersion(),
		Profile: in.Profile, Scenario: in.Scenario, Seed: in.Seed, Network: in.Network,
		Phases: in.Phases, Detectors: in.Detectors, Aggregate: in.Aggregate, Coverage: in.Coverage,
		Behaviour: in.Behaviour, Resources: in.Resources, Carrier: in.Carrier,
		BrowserProfiles: in.BrowserProfiles, Pcap: in.Pcap, Flows: in.Flows, Findings: in.Findings,
	}
}

// specLabel pins a report to a specification revision. An empty hash means the worktree has no
// git history (a copied tree); it is still better than a bare "v2" that compares as equal.
func specLabel(sha string) string {
	if sha == "" {
		return "v2"
	}
	return "v2@" + sha
}

// specVersion is the last commit that touched docs/protocol/v2, so two runs are comparable by the
// spec they were measured against, not just by the code commit.
func specVersion() string {
	sha, err := run("git", "log", "-1", "--format=%h", "--", "docs/protocol/v2")
	if err != nil {
		return specLabel("")
	}
	return specLabel(sha)
}

// recallText renders unavailable recall where JSON has null.
func recallText(r *float64) string {
	if r == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.2f", *r)
}

// commit returns the short hash of the run's tree and +dirty when tracked files are modified.
func commit() string {
	sha, err := run("git", "rev-parse", "--short", "HEAD")
	if err != nil || sha == "" {
		return "unknown"
	}
	if st, err := run("git", "status", "--porcelain", "--untracked-files=no"); err == nil && st != "" {
		sha += "+dirty"
	}
	return sha
}

// writeFlowsCSV writes the top flows by bytes. Detector scores come from LabeledFlow.Flags, which
// EvaluateDetectors fills; a missing score is "-" (the detector did not apply or did not run).
func writeFlowsCSV(w io.Writer, flows []censor.LabeledFlow, limit int) error {
	sorted := append([]censor.LabeledFlow(nil), flows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].BytesUp+sorted[i].BytesDn > sorted[j].BytesUp+sorted[j].BytesDn
	})
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()
	header := []string{"proto", "client", "server", "suite", "service", "bytes", "duration_ms",
		"first_size", "label", "looks", "sni", "ja3", "ja4", "verdict", "d1", "d2", "d3", "d4", "d10"}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, f := range sorted {
		ja3, ja4 := "", ""
		if f.Hello != nil {
			ja3, ja4 = f.Hello.JA3, f.Hello.JA4
		}
		row := []string{
			f.Proto, f.Client.String(), f.Server.String(), f.Suite, f.Service,
			strconv.Itoa(f.BytesUp + f.BytesDn), strconv.FormatInt(f.Duration.Milliseconds(), 10),
			strconv.Itoa(f.FirstSize), string(f.Label), f.Looks, f.SNI, ja3, ja4, f.Verdict(),
			score(f, "D1"), score(f, "D2"), score(f, "D3"), score(f, "D4"), score(f, "D10"),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	return cw.Error()
}

func score(f censor.LabeledFlow, id string) string {
	if f.Flags == nil {
		return "-"
	}
	v, ok := f.Flags[id]
	if !ok {
		return "-"
	}
	if v {
		return "1"
	}
	return "0"
}

// writeSummary is the one-page human report next to report.json.
func writeSummary(w io.Writer, r reportV2, flowLines []censor.LabeledFlow) {
	fmt.Fprintf(w, "EchoFog launchpad v2 — %s\n", r.Scenario)
	fmt.Fprintf(w, "when %s  commit %s  profile %s  network %s  seed %d\n", r.When, r.Commit, r.Profile, r.Network, r.Seed)
	fmt.Fprintf(w, "phases: %s\n\n", strings.Join(r.Phases, ", "))

	fmt.Fprintf(w, "carrier: %s (synthetic=%v)\n", r.Carrier.Name, r.Carrier.Synthetic)
	if r.Carrier.Note != "" {
		fmt.Fprintf(w, "  %s\n", r.Carrier.Note)
	}
	fmt.Fprintf(w, "flows: %d total, %d labelled (%d carrier, %d product, %d browser, %d tool)\n",
		r.Flows.Total, r.Flows.Labelled, r.Flows.Carrier, r.Flows.Product, r.Flows.Browser, r.Flows.Tool)
	fmt.Fprintf(w, "visibility: %.1f%% of carrier/product bytes are AT RISK; strict %.1f%%\n\n",
		r.Aggregate.VisibilityPct, r.Aggregate.StrictVisibilityPct)

	fmt.Fprintf(w, "detectors (recall / browser FPR):\n")
	var ids []string
	for id := range r.Detectors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := r.Detectors[id]
		fmt.Fprintf(w, "  %-3s recall %s  FPR %.3f  n=%d  positives=%d\n", id, recallText(m.Recall), m.FPR, m.N, m.Positives)
	}
	fmt.Fprintf(w, "\nbrowser profiles: version %s, %d entries\n", r.BrowserProfiles.Version, r.BrowserProfiles.Entries)
	if r.BrowserProfiles.Note != "" {
		fmt.Fprintf(w, "  %s\n", r.BrowserProfiles.Note)
	}
	fmt.Fprintf(w, "pcap: requested=%v budget=%s captured=%s kept=%v deleted=%v\n",
		r.Pcap.Requested, humanBytes(r.Pcap.BudgetBytes), humanBytes(r.Pcap.CapturedBytes), r.Pcap.Kept, r.Pcap.Deleted)

	if len(r.Findings) > 0 {
		fmt.Fprintf(w, "\nfindings:\n")
		for _, f := range r.Findings {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	} else {
		fmt.Fprintf(w, "\nfindings: none\n")
	}
	if len(flowLines) > 0 {
		fmt.Fprintf(w, "\ntop flows:\n")
		for _, f := range firstN(flowLines, 10) {
			fmt.Fprintf(w, "  %-4s %-22s %-14s %8s %-10s %s\n", f.Proto, f.Server.String(),
				humanBytes(int64(f.BytesUp+f.BytesDn)), f.Duration.Round(time.Second), string(f.Label), f.Verdict())
		}
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// saveResult keeps a copy outside testbed/out, which is pruned after seven days.
func saveResult(r reportV2, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := time.Now().Format("20060102-150405") + "-" + strings.ReplaceAll(r.Commit, "+", "_") + "-" + r.Profile + ".json"
	path := filepath.Join(dir, name)
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}
