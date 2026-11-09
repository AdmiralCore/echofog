package servernode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file opens the node's ports when the operator asked for it (T12.4f) and takes the rules
// back on a rollback or on uninstall. Only ufw and nftables are touched; anything else is left to
// the operator, who sees the commands in FirewallAdvice.

const nftTable = "echofog"

// firewallTool picks the host firewall: "ufw", "nft" or "" (unknown, nothing is touched).
func firewallTool(probe func(string) (string, error)) string {
	if probe == nil {
		return ""
	}
	if _, err := probe("ufw"); err == nil {
		return "ufw"
	}
	if _, err := probe("nft"); err == nil {
		return "nft"
	}
	return ""
}

// openFirewall opens the ports of cfg and returns the undo. A failure removes the rules it made.
func openFirewall(ctx context.Context, runner Runner, root string, cfg Config, tool string) (func(context.Context, Runner), error) {
	switch tool {
	case "ufw":
		var done []firewallRule
		undo := func(ctx context.Context, r Runner) {
			for i := len(done) - 1; i >= 0; i-- {
				removeUfwRule(ctx, r, done[i])
			}
		}
		for _, r := range firewallRules(cfg) {
			if err := addUfwRule(ctx, runner, r); err != nil {
				undo(ctx, runner)
				return nil, err
			}
			done = append(done, r)
		}
		return undo, nil
	case "nft":
		if err := writeNFTRules(ctx, runner, root, firewallRules(cfg)); err != nil {
			_, _ = runner.Run(ctx, "nft", "delete", "table", "inet", nftTable)
			_ = os.Remove(nftRulesPath(root))
			return nil, err
		}
		return func(ctx context.Context, r Runner) {
			_, _ = r.Run(ctx, "nft", "delete", "table", "inet", nftTable)
			_ = os.Remove(nftRulesPath(root))
		}, nil
	}
	return func(context.Context, Runner) {}, nil
}

func addUfwRule(ctx context.Context, runner Runner, r firewallRule) error {
	for _, proto := range []struct {
		on   bool
		name string
	}{{r.udp, "udp"}, {r.tcp, "tcp"}} {
		if !proto.on {
			continue
		}
		arg := fmt.Sprintf("%d/%s", r.port, proto.name)
		if out, err := runner.Run(ctx, "ufw", "allow", arg); err != nil {
			return fmt.Errorf("ufw allow %s: %w (%s)", arg, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func removeUfwRule(ctx context.Context, runner Runner, r firewallRule) {
	if r.udp {
		_, _ = runner.Run(ctx, "ufw", "delete", "allow", fmt.Sprintf("%d/udp", r.port))
	}
	if r.tcp {
		_, _ = runner.Run(ctx, "ufw", "delete", "allow", fmt.Sprintf("%d/tcp", r.port))
	}
}

// removeFirewall takes back what openFirewall added, driven by the configuration that is still on
// disk. Without a configuration only the nft table can be removed.
func removeFirewall(ctx context.Context, runner Runner, root string, cfg *Config, tool string) {
	switch tool {
	case "nft":
		_, _ = runner.Run(ctx, "nft", "delete", "table", "inet", nftTable)
		_ = os.Remove(nftRulesPath(root))
	case "ufw":
		if cfg == nil {
			return
		}
		for _, r := range firewallRules(*cfg) {
			removeUfwRule(ctx, runner, r)
		}
	}
}

func nftRulesPath(root string) string {
	return filepath.Join(root, filepath.FromSlash("/etc/nftables.d/echofog-node.nft"))
}

// writeNFTRules writes one small table of this node and loads it. The file stays for the host's
// own nftables.conf to include; uninstall removes both the table and the file.
func writeNFTRules(ctx context.Context, runner Runner, root string, rules []firewallRule) error {
	var b strings.Builder
	b.WriteString("table inet " + nftTable + " {\n")
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority filter; policy accept;\n")
	seen := map[string]bool{}
	for _, r := range rules {
		if r.udp && !seen[fmt.Sprintf("udp%d", r.port)] {
			seen[fmt.Sprintf("udp%d", r.port)] = true
			fmt.Fprintf(&b, "\t\tudp dport %d accept\n", r.port)
		}
		if r.tcp && !seen[fmt.Sprintf("tcp%d", r.port)] {
			seen[fmt.Sprintf("tcp%d", r.port)] = true
			fmt.Fprintf(&b, "\t\ttcp dport %d accept\n", r.port)
		}
	}
	b.WriteString("\t}\n}\n")
	path := nftRulesPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if out, err := runner.Run(ctx, "nft", "-f", filepath.ToSlash(path)); err != nil {
		return fmt.Errorf("nft -f: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultHealth polls the operator API of the started node until it answers or the deadline passes.
// It never prints a token: the client reads it from the data folder like every other command.
func defaultHealth(ctx context.Context, dataDir string) error {
	deadline := time.Now().Add(45 * time.Second)
	var last error
	for {
		c, err := newClient(dataDir)
		if err == nil {
			var st StatusInfo
			if err = c.call("GET", "/v1/status", nil, &st); err == nil {
				return nil
			}
		}
		last = err
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("the node did not answer within 45 s: %v", last)
}
