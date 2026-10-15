package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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
