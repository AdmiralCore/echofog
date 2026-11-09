package gate

import (
	"fmt"
	"strings"
)

// checklist builds the M4 checklist from the rows: every line has a status and the evidence it was
// read from, so nobody fills it in by hand.
func checklist(res Result) []Item {
	by := func(ids ...string) []Row {
		var out []Row
		for _, r := range res.Rows {
			for _, id := range ids {
				if r.ID == id {
					out = append(out, r)
				}
			}
		}
		return out
	}
	// summarize: red if any red, yellow if any yellow, not measured if all are, else green.
	summarize := func(rows []Row, evidence string) (Status, string) {
		if len(rows) == 0 {
			return NotMeasured, "no such row in the report"
		}
		st, measured := Green, 0
		var parts []string
		for _, r := range rows {
			if r.Status == NotMeasured || r.Status == Info {
				continue
			}
			measured++
			st = worse(st, r.Status)
			parts = append(parts, fmt.Sprintf("%s %s %s", r.ID, r.Metric, r.Value))
		}
		if measured == 0 {
			return NotMeasured, evidence
		}
		if len(parts) > 6 {
			parts = append(parts[:6], fmt.Sprintf("and %d more", len(parts)-6))
		}
		return st, strings.Join(parts, "; ")
	}
	only := func(rows []Row, metric string) []Row {
		var out []Row
		for _, r := range rows {
			if r.Metric == metric {
				out = append(out, r)
			}
		}
		return out
	}
	// a yellow false-positive rate is still not more than 1 %: only a red fails this line
	notRed := func(st Status) Status {
		if st == Yellow {
			return Green
		}
		return st
	}

	var items []Item
	add := func(text string, rows []Row, empty string, soften bool) {
		st, ev := summarize(rows, empty)
		if soften {
			st = notRed(st)
		}
		items = append(items, Item{Text: text, Status: st, Evidence: ev})
	}
	var all []Row
	for _, r := range res.Rows {
		all = append(all, r)
	}
	var fpr, controls []Row
	for _, r := range all {
		if r.Metric == "fpr" && r.ID != "D9" {
			fpr = append(fpr, r)
		}
		if r.Metric == "recall" && r.Source != "" && r.Status != Info {
			controls = append(controls, r)
		}
	}
	add("visibility_pct is 0 %", only(by("visibility"), "visibility_pct"), "no aggregate", false)
	add("no detector flags more than 1 % of the browser control", fpr, "no browser control", true)
	add("the controls are found (D1-D4, D10 recall 100 %, D5 at least 90 %)", controls, "no control flows", false)
	var v2 []Row
	for _, r := range res.Rows {
		if strings.HasPrefix(r.Metric, "v2") {
			v2 = append(v2, r)
		}
	}
	add("the v2 flows are not found: every detector flags at most 10 % of them, D6 AUC at most 0.65", v2, "no real v2 TCP flow in the run", false)
	add("H1a: the exit records are as far from the browser as the browser is from itself", by("H1a"), "no exit flow", false)
	add("no node answers a probe on its own outside the cover (D11-D15)", by("D11", "D12", "D13", "D14", "D15"), "no probe ran", false)
	add("no regression against the signed baseline", by("regression"), "no baseline", false)
	add("no carrier is more than 10 % slower than the T0.3 baseline (AD21)", by("speed"), "efv2 echo has not run", false)
	add("the backoff follows H15 (D24)", by("D24"), "no behaviour report", false)
	add("at most two carriers per (address, SNI): the AND rule does not fire (D21)", by("D21"), "no behaviour report", false)
	add("443 down: a backup port answers in 10 s (D25)", by("D25"), "no behaviour report", false)
	add("the product is fixed where the lab found it broken (freeze without a limit, PMTU)", by("product:freeze-no-limit", "product:noise3-pmtu"), "no behaviour report", false)
	var scen []Row
	for _, r := range res.Rows {
		if r.ID == "scenarios" || strings.HasPrefix(r.ID, "scenario:") && !strings.Contains(r.ID, "/") {
			scen = append(scen, r)
		}
	}
	add("T15.1: the scenarios (NAT, paths, hidden, messaging, call, exit, censor) are green three runs in a row, in the model and on the stand", scen, "no scenario report", false)
	return items
}

// RenderChecklist writes the M4 checklist as Markdown.
func RenderChecklist(r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# M4 checklist\n\nSigned %s, commit %s. Built from report.json by `launchpad-gate`; nobody filled it in.\n\n",
		r.SignedAt.UTC().Format("2006-01-02"), r.Commit)
	for _, it := range r.Checklist {
		box := "[ ]"
		if it.Status == Green {
			box = "[x]"
		}
		fmt.Fprintf(&b, "- %s %s — %s (%s)\n", box, it.Text, it.Status, it.Evidence)
	}
	fmt.Fprintf(&b, "\nVerdict: %s.\n\n## Policy\n\n", r.Verdict)
	for _, p := range r.Policy {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	if r.Blocks {
		b.WriteString("\n**The gate is red: this blocks the release.**\n")
		for _, w := range r.Waiting {
			fmt.Fprintf(&b, "- %s: %s. %s\n", w.ID, w.Value, w.Note)
		}
	}
	return b.String()
}
