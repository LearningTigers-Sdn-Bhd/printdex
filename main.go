// PrintDex is an event badge printing station: scan a ticket, the guest is
// checked in on EventzFlow and the badge prints.
package main

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/eventzflow"
	"github.com/Sienz16/printdex/internal/printer"
	"github.com/Sienz16/printdex/internal/server"
	"github.com/Sienz16/printdex/internal/update"
)

// Set at build time: -ldflags "-X main.version=1.2.3 -X main.updatePublicKey=<base64>".
var (
	version         = "dev"
	updatePublicKey = ""
)

func main() {
	if len(os.Args) > 1 {
		if err := runCommand(os.Args[1], os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	loadDotEnv()
	update.CleanUp()

	store := config.Open(config.DefaultPath(), config.LegacyPath())
	if cfg := store.Get(); cfg.Connected() {
		eventzflow.New(cfg).Warm()
	}
	app := &server.App{
		Store:   store,
		Printer: printer.New(),
		Updater: update.New(version, updatePublicKey),
		Version: version,
		// Loopback is the default; anything else means the operator opened it up.
		OnNetwork: envOr("HOST", "127.0.0.1") != "127.0.0.1",
	}
	handler := app.Handler()

	listener, err := listen()
	if err != nil {
		// The window still works without the port; only RfiDex's link is down.
		log.Printf("local API port unavailable: %v", err)
	} else {
		log.Printf("PrintDex %s listening on http://%s", version, listener.Addr())
		go func() { log.Fatal(http.Serve(listener, handler)) }()
	}
	if err := runWindow(handler, listener != nil); err != nil {
		log.Fatal(err)
	}
}

// runCommand handles the maintainer commands used when cutting a release.
func runCommand(name string, args []string) error {
	switch name {
	case "version":
		fmt.Println(version)
	case "keygen":
		public, private, err := update.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Printf("UPDATE_PUBLIC_KEY  (repository variable, baked into builds):\n%s\n", public)
		fmt.Printf("UPDATE_PRIVATE_KEY (repository secret, never commit):\n%s\n", private)
	case "sign":
		if len(args) != 1 {
			return fmt.Errorf("usage: printdex sign <file>   (key in UPDATE_PRIVATE_KEY)")
		}
		signature, err := update.Sign(os.Getenv("UPDATE_PRIVATE_KEY"), args[0])
		if err != nil {
			return err
		}
		fmt.Println(signature)
	default:
		return fmt.Errorf("unknown command %q (commands: version, keygen, sign)", name)
	}
	return nil
}

// listen binds the local API port. Loopback only by default: the API has no
// login, and RfiDex reaches it at http://127.0.0.1:8000. Set HOST=0.0.0.0 to
// expose it to the venue network on purpose. It retries briefly so a restart
// or update can take the port over from the copy that is still exiting.
func listen() (net.Listener, error) {
	address := net.JoinHostPort(envOr("HOST", "127.0.0.1"), envOr("PORT", "8000"))
	var err error
	for range 20 {
		var listener net.Listener
		if listener, err = net.Listen("tcp", address); err == nil {
			return listener, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, err
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// loadDotEnv reads KEY=VALUE lines from a .env file beside the executable,
// for settings such as PRINTER_NAME and PORT. Real environment variables win.
func loadDotEnv() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	file, err := os.Open(filepath.Join(filepath.Dir(exe), ".env"))
	if err != nil {
		return
	}
	defer file.Close()
	for lines := bufio.NewScanner(file); lines.Scan(); {
		key, value, found := strings.Cut(strings.TrimSpace(lines.Text()), "=")
		key = strings.TrimSpace(key)
		if !found || key == "" || strings.HasPrefix(key, "#") || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
	}
}
