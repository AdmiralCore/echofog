package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// PickAndSendFile lets the user choose a file and offers it to the contact with this public key.
// An empty result without error: the user closed the dialog.
func (a *App) PickAndSendFile(pubKey string) (*dto.FileTransferDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errors.New("node not started")
	}
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	path, err := runtime.OpenFileDialog(ctx, runtime.OpenDialogOptions{Title: "Send a file"})
	if err != nil || path == "" {
		return nil, err
	}
	t, err := nodeAPI.SendFile(pubKey, path)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SendFile offers a file (dropped on the window) to the contact with this public key.
func (a *App) SendFile(pubKey string, path string) (dto.FileTransferDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.FileTransferDTO{}, errors.New("node not started")
	}
	return nodeAPI.SendFile(pubKey, path)
}

func (a *App) AcceptFile(id string) (dto.FileTransferDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.FileTransferDTO{}, errors.New("node not started")
	}
	return nodeAPI.AcceptFile(id)
}

func (a *App) DeclineFile(id string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.DeclineFile(id)
}

func (a *App) CancelFile(id string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.CancelFile(id)
}

// ListFileTransfers is every transfer with a contact (peer ID), or all of them for "".
func (a *App) ListFileTransfers(peerID string) ([]dto.FileTransferDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errors.New("node not started")
	}
	return nodeAPI.ListFileTransfers(peerID)
}

// GetDownloadDir is where received files go.
func (a *App) GetDownloadDir() string {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return ""
	}
	return nodeAPI.DownloadDir()
}

// riskyExt are files that run code when opened: the UI asks before opening one.
var riskyExt = map[string]bool{
	".exe": true, ".msi": true, ".bat": true, ".cmd": true, ".com": true, ".scr": true, ".pif": true, ".cpl": true,
	".ps1": true, ".psm1": true, ".vbs": true, ".vbe": true, ".js": true, ".jse": true, ".wsf": true, ".wsh": true,
	".hta": true, ".jar": true, ".lnk": true, ".url": true, ".reg": true, ".dll": true, ".sys": true, ".msc": true,
	".appx": true, ".msix": true, ".iso": true, ".img": true, ".vhd": true, ".vhdx": true, ".app": true, ".sh": true,
	".command": true, ".desktop": true, ".dmg": true, ".pkg": true, ".deb": true, ".rpm": true, ".appimage": true,
}

// IsRiskyFile tells whether opening a file of this name runs a program.
func (a *App) IsRiskyFile(name string) bool {
	return riskyExt[strings.ToLower(filepath.Ext(name))]
}

// OpenTransferFile opens a received (or sent) file with the program the system has for it. A file
// that would run code opens only with allowRisky, after the user confirmed.
func (a *App) OpenTransferFile(id string, allowRisky bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	path, err := nodeAPI.FileTransferPath(id)
	if err != nil {
		return err
	}
	if a.IsRiskyFile(path) && !allowRisky {
		return errors.New("this file runs a program when opened")
	}
	return openWithSystem(path, false)
}

// ShowTransferFile opens the folder of a transfer's file with the file selected.
func (a *App) ShowTransferFile(id string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	path, err := nodeAPI.FileTransferPath(id)
	if err != nil {
		return err
	}
	return openWithSystem(path, true)
}

// OpenDownloadDir opens the folder received files go to.
func (a *App) OpenDownloadDir() error {
	dir := a.GetDownloadDir()
	if dir == "" {
		return errors.New("node not started")
	}
	return openWithSystem(dir, false)
}

// openWithSystem hands a path to the desktop: the path is one argument, never parsed by a shell.
func openWithSystem(path string, reveal bool) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "windows":
		if reveal {
			cmd = exec.Command("explorer.exe", "/select,", path)
		} else {
			cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", path)
		}
	case "darwin":
		if reveal {
			cmd = exec.Command("open", "-R", path)
		} else {
			cmd = exec.Command("open", path)
		}
	default:
		if reveal {
			path = filepath.Dir(path)
		}
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
