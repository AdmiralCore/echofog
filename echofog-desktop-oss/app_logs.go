package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/logging"
)

// GetLogs returns up to limit of the most recent captured log lines, oldest first (0 = all). The
// Logs view renders these; sensitive values are redacted and IP addresses are masked, so they are
// safer to share in a bug report.
func (a *App) GetLogs(limit int) []logging.Entry {
	return logging.Recent(limit)
}

// GetLogsText returns all retained log lines as one block of text, for copying into a bug report.
func (a *App) GetLogsText() string {
	return logging.Dump()
}

// GetLogFilePath returns the path of the on-disk log file.
func (a *App) GetLogFilePath() string {
	return logging.DefaultFilePath()
}

// ExportLogs writes the current logs to a timestamped file in the user's home directory and
// returns its path, so the user can attach it to a report.
func (a *App) ExportLogs() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	name := fmt.Sprintf("echofog-logs-%s.txt", time.Now().Format("2006-01-02-150405"))
	path := filepath.Join(home, name)
	if err := os.WriteFile(path, []byte(logging.Dump()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ConnectionReport is the anonymous connection report: what the node knows about how it connects,
// with no address, ID, link or contact in it. Nothing is sent; the user copies or saves it.
func (a *App) ConnectionReport() (api.ConnectionReport, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return api.ConnectionReport{}, errNodeNotStarted
	}
	return nodeAPI.ConnectionReport(version)
}

// SaveConnectionReport writes the report text to a timestamped file in the home directory.
func (a *App) SaveConnectionReport(text string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	path := filepath.Join(home, fmt.Sprintf("echofog-report-%s.txt", time.Now().Format("2006-01-02-150405")))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// streamLogName is what a stream log may be called: the app names it after the time it began.
var streamLogName = regexp.MustCompile(`^echofog-stream-[0-9-]{17}\.json$`)

// SaveStreamLog writes a stream log (the app puts it together while a stream plays) to the home
// directory under the given name and returns its path, for the user to keep or send into a chat.
func (a *App) SaveStreamLog(name, text string) (string, error) {
	if !streamLogName.MatchString(name) {
		return "", errors.New("bad stream log name")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	path := filepath.Join(home, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
