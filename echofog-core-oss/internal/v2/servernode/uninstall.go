package servernode

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UninstallDeps are the inputs of `v2 uninstall`.
type UninstallDeps struct {
	Root          string
	User          string
	DataDir       string
	Lang          string
	Purge         bool
	Runner        Runner
	Euid          int
	FirewallProbe func(name string) (string, error)
}

// Uninstall stops and removes the service, its unit, the binary and the firewall rules this
// installer added. The data folder (keys, the store, the recovery phrase file) stays unless Purge
// is set: losing the keys without a phrase in hand is not an uninstall, it is a deletion.
func Uninstall(ctx context.Context, d UninstallDeps) error {
	if !userName.MatchString(d.User) || d.User == "root" {
		return ErrBadUser
	}
	if d.Root == "" && d.Euid != 0 {
		return ErrNotRoot
	}
	if d.Runner == nil {
		return errors.New("servernode: no command runner")
	}
	dataDir := d.DataDir
	if dataDir == "" {
		dataDir = installData
	}
	at := func(p string) string { return filepath.Join(d.Root, filepath.FromSlash(p)) }

	// The service first: stopping it makes the rest safe to touch.
	_, _ = d.Runner.Run(ctx, "systemctl", "disable", "--now", unitServName)
	atData := at(dataDir)
	cfg := readConfigIfPresent(atData)
	probe := d.FirewallProbe
	if probe == nil {
		probe = exec.LookPath
	}
	if tool := firewallTool(probe); tool != "" {
		removeFirewall(ctx, d.Runner, d.Root, cfg, tool)
	}
	if err := os.Remove(at(installUnit)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = d.Runner.Run(ctx, "systemctl", "daemon-reload")
	if err := os.Remove(at(installBin)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if d.Purge {
		if err := os.RemoveAll(atData); err != nil {
			return err
		}
	}
	return nil
}

func readConfigIfPresent(dataDir string) *Config {
	raw, err := readLimited(filepath.Join(dataDir, configFileName), MaxConfigSize)
	if err != nil {
		return nil
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil
	}
	return cfg
}

// UninstallSummary is what the command reports: the paths it removed and the data it kept.
func UninstallSummary(d UninstallDeps) []string {
	lang := d.Lang
	dataDir := d.DataDir
	if dataDir == "" {
		dataDir = installData
	}
	out := []string{
		tr(lang, "Служба echofog-node остановлена и удалена, файл юнита удалён.", "The echofog-node service was stopped and removed, the unit file is gone."),
		tr(lang, "Правила файрвола, открытые установщиком, сняты.", "The firewall rules opened by the installer were removed."),
	}
	if d.Purge {
		out = append(out, tr(lang, "Данные удалены: "+dataDir, "The data was removed: "+dataDir))
	} else {
		out = append(out, tr(lang, "Данные сохранены: "+dataDir+". Удаление данных — только с --purge.", "The data is kept: "+dataDir+". Removing it needs --purge."))
	}
	return out
}

// PurgeConfirmation asks for a second, explicit word before the data folder is deleted.
func PurgeConfirmation(answer string) bool {
	return strings.EqualFold(strings.TrimSpace(answer), "delete")
}

// cmdUninstall is `echofog-node v2 uninstall`: the service and the firewall rules go, the data
// stays unless --purge is given and the operator confirms with the word "delete".
func cmdUninstall(args []string, env func(string) string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	root := fs.String("root", "", "install below this folder (tests)")
	user := fs.String("user", "echofog", "service user")
	data := dataFlag(fs, env)
	purge := fs.Bool("purge", false, "delete the data folder too (keys, store, phrase)")
	yes := fs.Bool("yes", false, "do not ask")
	langFlag := fs.String("lang", "", "ru or en")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError{fmt.Errorf("unexpected arguments: %v", pos)}
	}
	lang := langOf(*langFlag, env)
	dataDir := *data
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	if *purge && !*yes {
		fmt.Fprint(out, Message(lang, "uninstall.purge-confirm", dataDir)+" ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return usageError{ErrWizardInput}
		}
		if !PurgeConfirmation(line) {
			fmt.Fprintln(out, Message(lang, "uninstall.purge-declined"))
			return nil
		}
	}
	euid := os.Geteuid()
	if InstallEuid != nil {
		euid = InstallEuid()
	}
	runner := Runner(execRunner{})
	if InstallRunner != nil {
		runner = InstallRunner
	}
	d := UninstallDeps{Root: *root, User: *user, DataDir: dataDir, Lang: lang, Purge: *purge, Runner: runner, Euid: euid}
	if err := Uninstall(context.Background(), d); err != nil {
		return err
	}
	for _, line := range UninstallSummary(d) {
		fmt.Fprintln(out, line)
	}
	return nil
}
