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
	mime.AddExtensionType(".webm", "video/webm")

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "EchoFog",
		Width:     1100,
		Height:    720,
		MinWidth:  860,
		MinHeight: 560,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 14, G: 15, B: 17, A: 255},
		Frameless:        false,
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
