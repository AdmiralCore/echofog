package main

import (
	"errors"
	"os"

	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// SaveMessage bookmarks a copy of a message.
func (a *App) SaveMessage(m dto.SavedMessageDTO) (dto.SavedMessageDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return m, errors.New("node not started")
	}
	return nodeAPI.SaveMessage(m)
}

// UnsaveMessage removes a bookmark.
func (a *App) UnsaveMessage(id string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.UnsaveMessage(id)
}

// ListSavedMessages is every bookmark, the latest saved first.
func (a *App) ListSavedMessages() ([]dto.SavedMessageDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errors.New("node not started")
	}
	return nodeAPI.ListSavedMessages()
}

// SaveTextFile asks where to save text (a message) and writes it there. "" without error: the user
// closed the dialog.
func (a *App) SaveTextFile(suggestedName string, text string) (string, error) {
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	path, err := runtime.SaveFileDialog(ctx, runtime.SaveDialogOptions{
		DefaultFilename: suggestedName,
		Filters:         []runtime.FileFilter{{DisplayName: "Text (*.txt)", Pattern: "*.txt"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	return path, os.WriteFile(path, []byte(text), 0o600)
}
