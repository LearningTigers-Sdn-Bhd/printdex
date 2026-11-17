//go:build !desktop

package main

import (
	"errors"
	"net/http"
)

// Plain `go run .` build: no native window, just the server. Open
// http://127.0.0.1:8000 in a browser. Release builds use window.go.
func runWindow(_ http.Handler, listening bool) error {
	if !listening {
		return errors.New("the local API port is in use; is another copy running?")
	}
	select {}
}
