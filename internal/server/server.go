// Package server is the station's local HTTP API and dashboard.
//
// RfiDex calls /health and /scan/{id}/reprint, so those routes and their JSON
// shapes are a contract; the dashboard in ui/ uses the rest.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"image"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Sienz16/printdex/internal/badge"
	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/eventzflow"
	"github.com/Sienz16/printdex/internal/ticket"
	"github.com/Sienz16/printdex/internal/update"
)

//go:embed ui
var ui embed.FS

// previewDPI matches a typical label printer, so the preview is the print.
const previewDPI = 203

// Printer is what the server needs from the station's printer.
type Printer interface {
	Name() string
	Refresh() (installed []string, systemDefault string)
	Print(widthMM, heightMM float64, render func(dpi float64) *image.Gray) (jobID string, err error)
}

// App holds what the handlers work with.
type App struct {
	Store   *config.Store
	Printer Printer
	Updater *update.Updater
	Version string

	// OnNetwork is set when the station is deliberately reachable from other
	// devices (HOST=0.0.0.0), so requests may name any host.
	OnNetwork bool

	started time.Time
}

// apiError is a failure with the HTTP status to report it under.
type apiError struct {
	status int
	detail string
}

func (e apiError) Error() string { return e.detail }

func invalid(err error) error  { return apiError{http.StatusUnprocessableEntity, err.Error()} }
func upstream(err error) error { return apiError{http.StatusBadGateway, err.Error()} }
func printFailed(err error) error {
	return apiError{http.StatusInternalServerError, "Printing failed: " + err.Error()}
}

var errNotConnected = apiError{http.StatusBadRequest, "Backend not configured. Set backend URL, event slug, and API key."}

func writeJSON(w http.ResponseWriter, status int, reply any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(reply)
}

// writeError reports err as {"detail": "..."}, the shape the dashboard reads.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var apiErr apiError
	if errors.As(err, &apiErr) {
		status = apiErr.status
	}
	writeJSON(w, status, map[string]string{"detail": err.Error()})
}

// endpoint is a handler that returns its JSON reply, or an error.
type endpoint func(r *http.Request) (any, error)

func (h endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply, err := h(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// Handler returns the API and dashboard.
func (a *App) Handler() http.Handler {
	a.started = time.Now()
	mux := http.NewServeMux()

	dashboard, _ := fs.Sub(ui, "ui")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(dashboard)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, dashboard, "index.html")
	})
	mux.HandleFunc("GET /help", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, dashboard, "help.html")
	})

	for pattern, handler := range map[string]endpoint{
		"GET /health":          a.health,
		"GET /server/status":   a.serverStatus,
		"POST /server/restart": a.restart,
		"POST /server/quit":    a.quit,

		"GET /update/status":    func(*http.Request) (any, error) { return a.Updater.Status(), nil },
		"POST /update/check":    func(*http.Request) (any, error) { return a.Updater.Check(), nil },
		"POST /update/download": func(*http.Request) (any, error) { return a.Updater.Download(), nil },
		"POST /update/apply":    func(*http.Request) (any, error) { return a.Updater.Apply(), nil },

		"GET /config":    a.getConfig,
		"PUT /config":    a.putConfig,
		"DELETE /config": a.disconnect,

		"GET /printers":      a.printers,
		"POST /print-test":   a.printTest,
		"POST /print-ticket": a.printTicket,

		"GET /event":         a.eventInfo,
		"GET /ticket-types":  a.ticketTypes,
		"GET /ticket-groups": a.ticketGroups,
		"GET /search":        a.search,

		"POST /scan/{public_id}":         a.scan,
		"POST /scan/{public_id}/reprint": a.reprint,
	} {
		mux.Handle(pattern, handler)
	}
	mux.HandleFunc("POST /png-preview", a.preview)
	return guard(mux, a.OnNetwork)
}

// guard keeps web pages in the operator's browser from driving the station,
// and caps request bodies. Two checks:
//
//   - Host: unless the station is on the network on purpose, the request must
//     name a local host. A page that tricks the browser into resolving its own
//     domain to 127.0.0.1 (DNS rebinding) arrives with its own domain here, so
//     it can't even read the config.
//   - Origin: state-changing requests must come from a local page.
func guard(next http.Handler, onNetwork bool) http.Handler {
	trusted := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "wails": true, "wails.localhost": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host := r.Host; !onNetwork {
			if name, _, err := net.SplitHostPort(host); err == nil {
				host = name
			}
			if !trusted[strings.Trim(host, "[]")] {
				writeError(w, apiError{http.StatusForbidden, "Requests must use a local address."})
				return
			}
		}
		if origin := r.Header.Get("Origin"); origin != "" && r.Method != http.MethodGet {
			if u, err := url.Parse(origin); err != nil || !trusted[u.Hostname()] {
				writeError(w, apiError{http.StatusForbidden, "Cross-site requests are not allowed."})
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// --- station ---

func (a *App) health(*http.Request) (any, error) {
	return map[string]any{"ok": true, "printer": a.Printer.Name(), "version": a.Version}, nil
}

func (a *App) serverStatus(*http.Request) (any, error) {
	return map[string]any{
		"ok": true, "pid": os.Getpid(),
		"uptime_seconds": int(time.Since(a.started).Seconds()),
		"started_at":     float64(a.started.UnixMilli()) / 1000,
	}, nil
}

// afterReply runs action once the HTTP reply has had time to reach the dashboard.
func afterReply(action func()) {
	go func() {
		time.Sleep(400 * time.Millisecond)
		action()
	}()
}

func (a *App) restart(*http.Request) (any, error) {
	afterReply(func() {
		if err := update.Relaunch(); err != nil {
			log.Printf("restart: %v", err) // stay open rather than quit with nothing to replace us
			return
		}
		os.Exit(0)
	})
	return map[string]any{"ok": true, "action": "restart"}, nil
}

func (a *App) quit(*http.Request) (any, error) {
	afterReply(func() { os.Exit(0) })
	return map[string]any{"ok": true, "action": "quit"}, nil
}

// --- config ---

func (a *App) getConfig(*http.Request) (any, error) {
	return a.Store.Get().Public(a.Store.Path()), nil
}

func (a *App) putConfig(r *http.Request) (any, error) {
	var patch config.Patch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		return nil, invalid(errors.New("settings are not in the expected format"))
	}
	if patch.Layout != nil {
		if _, err := patch.Layout.Normalize(); err != nil {
			return nil, invalid(err)
		}
	}
	return a.save(patch)
}

// disconnect forgets the event connection; the layout stays.
func (a *App) disconnect(*http.Request) (any, error) {
	empty := ""
	return a.save(config.Patch{BackendURL: &empty, EventSlug: &empty, APIKey: &empty})
}

func (a *App) save(patch config.Patch) (any, error) {
	cfg, err := a.Store.Save(patch)
	if err != nil {
		return nil, errors.New("Could not save settings: " + err.Error())
	}
	if cfg.Connected() {
		eventzflow.New(cfg).Warm()
	}
	return cfg.Public(a.Store.Path()), nil
}

// --- printing ---

type printJob struct {
	JobID   string `json:"job_id"`
	Printer string `json:"printer"`
}

// print sends one page to the printer, drawn by render at the printer's resolution.
func (a *App) print(cfg config.Config, render func(dpi float64) *image.Gray) (*printJob, error) {
	paper := cfg.Layout.Paper
	jobID, err := a.Printer.Print(paper.WidthMM, paper.HeightMM, func(dpi float64) *image.Gray {
		page := render(dpi)
		if cfg.DirectThermal {
			page = badge.Thicken(page)
		}
		return page
	})
	if err != nil {
		return nil, err
	}
	return &printJob{JobID: jobID, Printer: a.Printer.Name()}, nil
}

func (a *App) printBadge(cfg config.Config, t ticket.Ticket) (*printJob, error) {
	return a.print(cfg, func(dpi float64) *image.Gray { return badge.Render(t, cfg.Layout, dpi) })
}

func (a *App) printers(*http.Request) (any, error) {
	installed, systemDefault := a.Printer.Refresh()
	return map[string]any{"printers": installed, "default_printer": systemDefault, "configured_printer": a.Printer.Name()}, nil
}

func (a *App) printTest(*http.Request) (any, error) {
	cfg := a.Store.Get()
	job, err := a.print(cfg, func(dpi float64) *image.Gray { return badge.TestPage(cfg.Layout, dpi) })
	if err != nil {
		return nil, printFailed(err)
	}
	return map[string]any{"ok": true, "print_job": job, "target_printer": job.Printer}, nil
}

// printTicket prints a badge from details typed into the dashboard (or sent
// by another local app). No backend involved.
func (a *App) printTicket(r *http.Request) (any, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, invalid(errors.New("request is too large"))
	}
	t, err := ticket.Parse(body)
	if err != nil {
		return nil, invalid(err)
	}
	job, err := a.printBadge(a.Store.Get(), t)
	if err != nil {
		return nil, printFailed(err)
	}
	return map[string]any{"ok": true, "printed": true, "print_job": job, "target_printer": job.Printer}, nil
}

// preview answers with the badge as a PNG. The layout editor sends its
// unsaved layout along; without one the saved layout is used.
func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ticket json.RawMessage `json:"ticket"`
		Layout *config.Layout  `json:"layout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, invalid(errors.New("preview request is not in the expected format")))
		return
	}
	t, err := ticket.Parse(body.Ticket)
	if err != nil {
		writeError(w, invalid(err))
		return
	}
	layout := a.Store.Get().Layout
	if body.Layout != nil {
		if draft, err := body.Layout.Normalize(); err == nil {
			layout = draft
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(badge.PNG(badge.Render(t, layout, previewDPI), previewDPI))
}

// --- event data ---

// event returns the config and a client for the connected event.
func (a *App) event(needsKey bool) (config.Config, *eventzflow.Client, error) {
	cfg := a.Store.Get()
	if !cfg.Connected() || (needsKey && cfg.APIKey == "") {
		return cfg, nil, errNotConnected
	}
	return cfg, eventzflow.New(cfg), nil
}

// eventInfo says which event the station is connected to, so the operator can
// confirm that the ID or slug they entered points where they meant.
func (a *App) eventInfo(*http.Request) (any, error) {
	_, client, err := a.event(false)
	if err != nil {
		return nil, err
	}
	info, err := client.Event()
	if err != nil {
		return nil, upstream(err)
	}
	return map[string]any{"ok": true, "id": info.ID, "title": info.Title, "slug": info.Slug}, nil
}

func (a *App) ticketTypes(*http.Request) (any, error) {
	_, client, err := a.event(true)
	if err != nil {
		return nil, err
	}
	types, err := client.TicketTypes()
	if err != nil {
		return nil, upstream(err)
	}
	return map[string]any{"ok": true, "types": types}, nil
}

func (a *App) ticketGroups(*http.Request) (any, error) {
	_, client, err := a.event(true)
	if err != nil {
		return nil, err
	}
	enabled, groups, err := client.TicketGroups()
	if err != nil {
		return nil, upstream(err)
	}
	return map[string]any{"ok": true, "enabled": enabled, "groups": groups}, nil
}

var (
	nonDigits  = regexp.MustCompile(`\D`)
	phoneChars = regexp.MustCompile(`^[\d\s+()\-]+$`)
)

// searchMethod decides what a search box entry is, by the rules of the
// EventzFlow check-in page: an '@' means email, digits mean phone, anything
// else is a name. It returns "" while the entry is too short to search.
func searchMethod(query string) string {
	length := len([]rune(query))
	digits := len(nonDigits.ReplaceAllString(query, ""))
	switch {
	case strings.Contains(query, "@"):
		if length >= 5 {
			return "email"
		}
	case digits > 0 && phoneChars.MatchString(query):
		if digits >= 4 {
			return "phone"
		}
	case length >= 2:
		return "name"
	}
	return ""
}

// search finds guests by name, email or phone for when a QR code can't be
// scanned. The operator picks one, which then goes through scan as usual.
func (a *App) search(r *http.Request) (any, error) {
	_, client, err := a.event(false)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if runes := []rune(query); len(runes) > 100 {
		query = string(runes[:100])
	}
	method := searchMethod(query)
	if method == "" {
		return map[string]any{"ok": true, "method": nil, "results": []any{}}, nil
	}
	guests, err := client.Search(method, query)
	if err != nil {
		return nil, upstream(err)
	}
	return map[string]any{"ok": true, "method": method, "results": guests}, nil
}

// lookup fetches a ticket and maps it for printing.
func lookup(cfg config.Config, client *eventzflow.Client, publicID string) (ticket.Ticket, error) {
	data, err := client.FetchTicket(publicID)
	if err != nil {
		return ticket.Ticket{}, upstream(err)
	}
	t, err := data.Ticket(cfg.Layout.CustomFields)
	if err != nil {
		return ticket.Ticket{}, upstream(errors.New("Invalid ticket data from backend: " + err.Error()))
	}
	return t, nil
}

// Scan outcomes, as the dashboard knows them.
const (
	statusPrinted     = "printed"      // checked in, badge sent to the printer
	statusNoBadge     = "no_badge"     // checked in; this ticket type gets no badge
	statusPrintFailed = "print_failed" // checked in, but the printer refused; the operator can reprint
	statusAlready     = "already"      // checked in earlier; nothing printed
)

type scanResult struct {
	OK             bool            `json:"ok"`
	Status         string          `json:"status"`
	AlreadyScanned bool            `json:"already_scanned"`
	Ticket         ticket.Ticket   `json:"ticket"`
	CheckIn        json.RawMessage `json:"check_in,omitempty"`
	PrintJob       *printJob       `json:"print_job,omitempty"`
	PrintError     string          `json:"print_error,omitempty"`

	// Set when the ticket was already checked in.
	CheckInMessage    string `json:"check_in_message,omitempty"`
	CheckedInAt       string `json:"checked_in_at,omitempty"`
	CheckedInBy       string `json:"checked_in_by,omitempty"`
	CheckedInLocation string `json:"checked_in_location,omitempty"`

	TimingMS map[string]int64 `json:"timing_ms"` // where the time went, for tuning stations
}

// scan is the main event: look the ticket up, check the guest in, print.
func (a *App) scan(r *http.Request) (any, error) {
	cfg, client, err := a.event(true)
	if err != nil {
		return nil, err
	}
	timing := map[string]int64{}
	timed := func(step string, start time.Time) { timing[step] = time.Since(start).Milliseconds() }
	begin := time.Now()

	t, err := lookup(cfg, client, r.PathValue("public_id"))
	if err != nil {
		return nil, err
	}
	timed("lookup", begin)
	result := scanResult{OK: true, Ticket: t, TimingMS: timing}

	step := time.Now()
	checkIn, err := client.CheckIn(t.ID)
	timed("check_in", step)

	var already *eventzflow.AlreadyCheckedIn
	switch {
	case errors.As(err, &already):
		result.Status, result.AlreadyScanned = statusAlready, true
		result.CheckInMessage, result.CheckedInAt = already.Message, already.At
		result.CheckedInBy, result.CheckedInLocation = already.By, already.Location
	case err != nil:
		return nil, upstream(err)
	case cfg.SkipsBadge(t.Type):
		result.Status, result.CheckIn = statusNoBadge, checkIn
	default:
		// The guest is checked in now, so a printer fault is reported as an
		// outcome, not an error: scanning again would only say "already".
		result.CheckIn = checkIn
		step = time.Now()
		job, err := a.printBadge(cfg, t)
		timed("print", step)
		if err != nil {
			result.Status, result.PrintError = statusPrintFailed, "Printing failed: "+err.Error()
		} else {
			result.Status, result.PrintJob = statusPrinted, job
		}
	}
	timed("total", begin)
	log.Printf("scan %s: %s %v", t.ID, result.Status, timing)
	return result, nil
}

// reprint prints a badge on the operator's request: no check-in, and it
// prints even when the ticket type normally gets no badge.
func (a *App) reprint(r *http.Request) (any, error) {
	cfg, client, err := a.event(false)
	if err != nil {
		return nil, err
	}
	t, err := lookup(cfg, client, r.PathValue("public_id"))
	if err != nil {
		return nil, err
	}
	job, err := a.printBadge(cfg, t)
	if err != nil {
		return nil, printFailed(err)
	}
	return map[string]any{"ok": true, "reprinted": true, "ticket": t, "print_job": job}, nil
}
