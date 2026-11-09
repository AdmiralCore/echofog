// The v1 REALITY QUIC dial predicts its key share from tls.Config.Rand. Since Go 1.24
// crypto/ecdh ignores that reader unless custom randomness is enabled, so the v1 client needs
// this build-time default. The v2 client (T12.6) no longer depends on it.
//
//go:debug cryptocustomrand=1
package main

import (
	"embed"
	"mime"
	"os"

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
	if len(os.Args) > 1 && os.Args[1] == api.KillSwitchHelperFlag {
		// started elevated by the kill switch of protocol 2: no window, no node
		os.Exit(api.RunKillSwitchHelper(os.Args[2:]))
	}
	mime.AddExtensionType(".webm", "video/webm")
	window := len(os.Args) > 1 && os.Args[1] == appChildFlag // started by the watcher
	if window {
		os.Args = append(os.Args[:1:1], os.Args[2:]...)
	}

	// started by a clicked echofog:// link: an EchoFog window that already runs takes it
	if link := linkFromArgs(os.Args[1:]); link != "" {
		if handOffLink(link) {
			return
		}
		startLinkMu.Lock()
		startLink = link
		startLinkMu.Unlock()
	}
	if !window && watch() {
		return // the window ran in a watched copy of this program, and it is closed
	}
	// started by the system at sign-in (app_autostart.go): the window opens minimised
	startState := options.Normal
	if args, ok := takeAutostartFlag(os.Args); ok {
		os.Args = args
		startState = options.Minimised
	}
	applyProtocolFlag() // --protocol 2: the mode of this run (T11.7)
	keepCrashes()
	crashTest(os.Getenv("ECHOFOG_CRASH_TEST"))
	registerLinkScheme()

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "EchoFog",
		Width:     1100,
		Height:    720,
		MinWidth:  420,
		MinHeight: 520,
		// a minimised window still loads the page, so a remembered profile opens and the node starts
		WindowStartState: startState,
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
