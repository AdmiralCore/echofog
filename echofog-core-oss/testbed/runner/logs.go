package main

// After every run the logs of all containers are kept, and read for what should not be there.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	outDir    = flag.String("out", "", "where logs and reports go (default testbed/out/<time>-<scenario>)")
	keepPcaps = flag.Int("keep-pcaps", 3, "runs under testbed/out whose packet captures are kept; older runs keep only their logs and reports (0: keep all)")
)

func prepareOut() string {
	dir := *outDir
	if dir == "" {
		base := filepath.Join("testbed", "out")
		prunePcaps(base, *keepPcaps)
		dir = filepath.Join(base, time.Now().Format("20060102-150405")+"-"+*scenario)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("out dir: %v", err)
	}
	return dir
}

var (
	// a log line reduced to its kind: ids, addresses, numbers and durations go
	reHex   = regexp.MustCompile(`[0-9a-f]{12,}`)
	reAddr  = regexp.MustCompile(`\d{1,3}(\.\d{1,3}){3}(:\d+)?|\[[0-9a-f:]+\](:\d+)?`)
	reNum   = regexp.MustCompile(`\b\d+(\.\d+)?(ms|µs|s|m|h|MiB|KiB|B|%)?\b`)
	reTime  = regexp.MustCompile(`^(time=\S+ |\d{4}/\d\d/\d\d \d\d:\d\d:\d\d )`)
	badWord = regexp.MustCompile(`panic|fatal error|DATA RACE|level=ERROR|concurrent map|deadlock|nil pointer|index out of range|goroutine \d+ \[`)
)

func kindOf(line string) string {
	line = reTime.ReplaceAllString(line, "")
	line = reHex.ReplaceAllString(line, "<id>")
	line = reAddr.ReplaceAllString(line, "<addr>")
	line = reNum.ReplaceAllString(line, "<n>")
	if len(line) > 170 {
		line = line[:170]
	}
	return line
}

// collectLogs saves the log of every container and writes a summary of what stands out: crashes,
// errors, and the warnings that came most often.
func collectLogs(dir string) {
	out, err := run("docker", "ps", "-a", "--filter", "name="+project+"-", "--format", "{{.Names}}")
	if err != nil {
		logf("cannot list containers: %v", err)
		return
	}
	var sum strings.Builder
	var alarms []string
	warnKinds := map[string]int{}
	warnWhere := map[string]map[string]bool{}
	restarts := map[string]int{}
	for _, c := range strings.Fields(out) {
		logs, _ := run("docker", "logs", "--timestamps", c)
		_ = os.WriteFile(filepath.Join(dir, c+".log"), []byte(logs), 0o644)
		short := strings.TrimPrefix(c, project+"-")
		for _, line := range strings.Split(logs, "\n") {
			if i := strings.IndexByte(line, ' '); i > 0 && strings.HasPrefix(line, "20") {
				line = line[i+1:] // docker's own timestamp
			}
			switch {
			case badWord.MatchString(line):
				alarms = append(alarms, short+": "+strings.TrimSpace(line))
			case strings.Contains(line, "level=WARN"):
				k := kindOf(line)
				warnKinds[k]++
				if warnWhere[k] == nil {
					warnWhere[k] = map[string]bool{}
				}
				warnWhere[k][short] = true
			}
			if strings.Contains(line, "testnode ") && strings.Contains(line, " ready: id=") {
				restarts[short]++
			}
		}
	}

	fmt.Fprintf(&sum, "logs of %s\n\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&sum, "== crashes and errors (%d)\n", len(alarms))
	for _, a := range firstN(alarms, 60) {
		fmt.Fprintf(&sum, "  %s\n", a)
	}
	fmt.Fprintf(&sum, "\n== starts per container (more than 1: it was restarted or crashed)\n")
	var names []string
	for n := range restarts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&sum, "  %-6s %d\n", n, restarts[n])
	}
	type kc struct {
		k string
		n int
	}
	var kinds []kc
	for k, n := range warnKinds {
		kinds = append(kinds, kc{k, n})
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].n > kinds[j].n })
	fmt.Fprintf(&sum, "\n== warnings by kind (%d kinds)\n", len(kinds))
	for _, x := range firstN(kinds, 40) {
		var where []string
		for w := range warnWhere[x.k] {
			where = append(where, w)
		}
		sort.Strings(where)
		fmt.Fprintf(&sum, "  %6d  [%s]  %s\n", x.n, strings.Join(where, ","), x.k)
	}
	_ = os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(sum.String()), 0o644)

	logf("logs and summary: %s", dir)
	logf("crashes and errors in the logs: %d", len(alarms))
	for _, a := range firstN(alarms, 10) {
		logf("  ! %.200s", a)
	}
	logf("the most frequent warnings:")
	for _, x := range firstN(kinds, 8) {
		logf("  %6d  %.150s", x.n, x.k)
	}
}

// prunePcaps deletes the packet captures of all but the newest keep runs under base, before a new
// run starts. A censor run records every packet of every gateway, a gigabyte or more, and a day of
// runs filled the disk; the reports and logs, a few hundred KB a run, stay. keep 0: nothing goes.
func prunePcaps(base string, keep int) {
	if keep <= 0 {
		return
	}
	runs, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var dirs []string
	for _, r := range runs {
		if r.IsDir() {
			dirs = append(dirs, r.Name())
		}
	}
	sort.Strings(dirs) // the names start with the time of the run
	if len(dirs) < keep {
		return
	}
	var freed int64
	for _, d := range dirs[:len(dirs)-(keep-1)] { // the new run makes keep
		_ = filepath.WalkDir(filepath.Join(base, d), func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			if ext := strings.ToLower(filepath.Ext(p)); ext == ".pcap" || ext == ".pcapng" {
				if fi, err := e.Info(); err == nil && os.Remove(p) == nil {
					freed += fi.Size()
				}
			}
			return nil
		})
	}
	if freed > 0 {
		logf("removed the packet captures of older runs: %d MB freed (-keep-pcaps %d)", freed>>20, keep)
	}
}
