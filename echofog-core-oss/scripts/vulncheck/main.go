// Command vulncheck runs govulncheck and fails only on vulnerabilities that are not listed in
// .govulncheck-ignore (one id per line, "GO-2026-0001  why it is accepted"). govulncheck itself has
// no way to accept a known, understood finding.
//
//	go run ./scripts/vulncheck
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func main() {
	ignored := map[string]string{}
	if f, err := os.Open(".govulncheck-ignore"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			id, why, _ := strings.Cut(line, " ")
			ignored[id] = strings.TrimSpace(why)
		}
		f.Close()
	}

	cmd := exec.Command("go", "run", "golang.org/x/vuln/cmd/govulncheck@latest", "./...")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := out.String()
	if err != nil && !strings.Contains(text, "Vulnerability #") && !strings.Contains(text, "No vulnerabilities found") {
		fmt.Print(text)
		fmt.Fprintln(os.Stderr, "govulncheck did not run:", err)
		os.Exit(2)
	}

	re := regexp.MustCompile(`(?m)^Vulnerability #\d+: (GO-\d+-\d+)\n(.*(?:\n    .*)*)`)
	failed := 0
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		id := m[1]
		if why, ok := ignored[id]; ok {
			fmt.Printf("accepted  %s  %s\n", id, why)
			continue
		}
		failed++
		fmt.Printf("VULNERABLE %s\n%s\n", id, m[0])
	}
	if failed > 0 {
		fmt.Printf("\n%d vulnerabilities that the code reaches and nobody accepted\n", failed)
		os.Exit(1)
	}
	fmt.Println("no unaccepted vulnerabilities")
}
