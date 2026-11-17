//go:build desktop

package main

import (
	"errors"
	"net/http"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// runWindow opens the native window (WebView2 on Windows, WKWebView on macOS).
// The dashboard is served straight from handler, so the window works even if
// the local API port is taken. HEADLESS=1 runs the server alone.
func runWindow(handler http.Handler, listening bool) error {
	if os.Getenv("HEADLESS") != "" {
		if !listening {
			return errors.New("headless mode needs the local API port")
		}
		select {}
	}
	return wails.Run(&options.App{
		Title:       "PrintDex",
		Width:       960,
		Height:      720,
		MinWidth:    720,
		MinHeight:   560,
		AssetServer: &assetserver.Options{Handler: handler},
	})
}
