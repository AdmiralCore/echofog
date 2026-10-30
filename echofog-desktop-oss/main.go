package main

import (
	"embed"
	"mime"
	"os"
	"path/filepath"
	"runtime/debug"

	api "github.com/echofog/echofog-core/pkg/api/node"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	if len(os.Args) > 1 && os.Args[1] == api.TunHelperFlag {
		// started elevated by the node for whole-computer mode: no window, no node
		os.Exit(api.RunTunHelper(os.Args[2:]))
	}
	mime.AddExtensionType(".webm", "video/webm")
	keepCrashes()

	// started by a clicked echofog:// link: an EchoFog window that already runs takes it
	if link := linkFromArgs(os.Args[1:]); link != "" {
		if handOffLink(link) {
			return
		}
		startLinkMu.Lock()
		startLink = link
		startLinkMu.Unlock()
	}
	registerLinkScheme()

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "EchoFog",
		Width:     1100,
		Height:    720,
		MinWidth:  420,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 11, G: 12, B: 16, A: 255},
		// files dropped on a chat are sent; the web view itself never opens a dropped file
		DragAndDrop: &options.DragAndDrop{EnableFileDrop: true, DisableWebViewDrop: true},
		Frameless:   false,
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHidden(),
			Appearance:           mac.NSAppearanceNameDarkAqua,
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			About: &mac.AboutInfo{
				Title:   "EchoFog",
				Message: "Peer-to-peer messenger, calls and proxy",
			},
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			BackdropType:         windows.None,
			DisableWindowIcon:    false,
			WebviewGpuIsDisabled: false,
		},
		Linux: &linux.Options{
			WindowIsTranslucent: false,
		},
		OnStartup: app.startup,
		// close the database properly: a process killed while it writes leaves a broken table
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

// keepCrashes writes a crash (a panic nobody recovered, a fatal runtime error) to crash.log next
// to the app's log: a windowed app has no console, so the trace was lost and a crash at login
// left nothing to go on.
func keepCrashes() {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "EchoFog")
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, "crash.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	_ = f.Close() // SetCrashOutput keeps its own copy
}
