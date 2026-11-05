package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// compareReports prints the metrics of two runs in two columns, with * on every changed value, and
// the findings of both. It is the same shape as the v1 launchpad compare, but reads report.json v2.
func compareReports(a, b []byte) (string, error) {
	var ma, mb map[string]any
	if err := json.Unmarshal(a, &ma); err != nil {
		return "", fmt.Errorf("first report: %w", err)
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return "", fmt.Errorf("second report: %w", err)
	}
	fa, fb := map[string]string{}, map[string]string{}
	flatten("", ma, fa)
	flatten("", mb, fb)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	var ks []string
	for k := range keys {
		switch {
		case k == "when", k == "commit", k == "findings",
			k == "pcap.captured_bytes", k == "pcap.files", k == "pcap.kept", k == "pcap.deleted", k == "pcap.reason":
			continue // run identity, findings (printed separately) and raw capture size are not metrics
		}
		ks = append(ks, k)
	}
	sort.Strings(ks)
	w := 0
	for _, k := range ks {
		w = max(w, len(k))
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-*s  %-28s  %-28s\n", w, "metric", "before", "after")
	for _, k := range []string{"scenario", "profile", "network", "seed",
		"aggregate.visibility_pct", "aggregate.strict_visibility_pct",
		"detectors.D1.recall", "detectors.D1.fpr", "detectors.D2.recall", "detectors.D2.fpr",
		"detectors.D3.recall", "detectors.D3.fpr", "detectors.D4.recall", "detectors.D4.fpr",
		"detectors.D10.recall", "detectors.D10.fpr"} {
		if _, ok := keys[k]; !ok {
			continue
		}
		mark := ""
		if fa[k] != fb[k] {
			mark = "  *"
		}
		fmt.Fprintf(&out, "%-*s  %-28.28s  %-28.28s%s\n", w, k, fa[k], fb[k], mark)
	}
	fmt.Fprintf(&out, "%s\n", strings.Repeat("-", w+60))
	for _, k := range ks {
		mark := ""
		if fa[k] != fb[k] {
			mark = "  *"
		}
		fmt.Fprintf(&out, "%-*s  %-28.28s  %-28.28s%s\n", w, k, fa[k], fb[k], mark)
	}
	fmt.Fprintf(&out, "\nfindings before:\n")
	printJSONList(&out, ma["findings"])
	fmt.Fprintf(&out, "\nfindings after:\n")
	printJSONList(&out, mb["findings"])
	return out.String(), nil
}

func printJSONList(out *strings.Builder, v any) {
	list, _ := v.([]any)
	if len(list) == 0 {
		fmt.Fprintf(out, "  (none)\n")
		return
	}
	for _, e := range list {
		fmt.Fprintf(out, "  - %v\n", e)
	}
}

func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, vv, out)
		}
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = fmt.Sprint(e)
		}
		out[prefix] = strings.Join(parts, ",")
	default:
		if x == nil {
			out[prefix] = "null"
			return
		}
		out[prefix] = fmt.Sprint(x)
	}
}
