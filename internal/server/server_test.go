package server

import (
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/update"
)

// fakePrinter records print jobs instead of printing them.
type fakePrinter struct {
	jobs  int
	fault error
}

func (p *fakePrinter) Name() string                { return "test-printer" }
func (p *fakePrinter) Refresh() ([]string, string) { return []string{"test-printer"}, "test-printer" }
func (p *fakePrinter) Print(_, _ float64, render func(float64) *image.Gray) (string, error) {
	if p.fault != nil {
		return "", p.fault
	}
	render(203)
	p.jobs++
	return "job-1", nil
}

// fakeEventzFlow serves one ticket per id; check-in answers depend on the id.
func fakeEventzFlow() *httptest.Server {
	reply := func(w http.ResponseWriter, status int, body any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Base(strings.TrimSuffix(r.URL.Path, "/check_in"))
		switch {
		case strings.Contains(r.URL.Path, "/tickets/"):
			ticketType := "Delegate"
			if id == "STAFF" {
				ticketType = "Staff"
			}
			reply(w, 200, map[string]any{"data": map[string]any{
				"public_id": id, "attendee_name": "Aisha Tan", "ticket_type": ticketType,
				"ticket_group":       map[string]any{"id": 3, "name": "Room A"},
				"custom_fields_data": map[string]any{"organisation": "Acme", "Sponsor Tier": "Gold"}}})
		case r.Method != "PATCH":
			reply(w, 404, nil)
		case r.Header.Get("Authorization") != "key":
			reply(w, 401, nil)
		case id == "DUPE":
			reply(w, 422, map[string]any{"error": "Already checked in", "checked_in_at": "2026-11-17T09:00:00Z",
				"blocked_by": map[string]any{"scanned_by_name": "Gate 2"}})
		case id == "UNPAID":
			reply(w, 422, map[string]any{"error": "Ticket is not paid"})
		default:
			reply(w, 200, map[string]any{"ok": true})
		}
	}))
}

func TestScanFlow(t *testing.T) {
	backend := fakeEventzFlow()
	defer backend.Close()

	store := config.Open(filepath.Join(t.TempDir(), "config.json"))
	layout := config.DefaultLayout()
	layout.Elements = append(layout.Elements, "custom_tier")
	layout.CustomFields["custom_tier"] = config.CustomField{Label: "sponsor tier"} // matched to the backend by label
	key := "key"
	if _, err := store.Save(config.Patch{BackendURL: &backend.URL, EventSlug: &key, APIKey: &key,
		NoPrintTicketTypes: []string{"Staff"}, Layout: &layout}); err != nil {
		t.Fatal(err)
	}
	printer := &fakePrinter{}
	app := httptest.NewServer((&App{Store: store, Printer: printer, Updater: update.New("dev", "")}).Handler())
	defer app.Close()

	post := func(path, origin string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", app.URL+path, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	for _, tc := range []struct {
		id, status string
		jobs       int
	}{
		{"A1", statusPrinted, 1},
		{"DUPE", statusAlready, 0},  // checked in earlier: must not print again
		{"STAFF", statusNoBadge, 0}, // type on the skip list: check in only
	} {
		printer.jobs = 0
		code, body := post("/scan/"+tc.id, "")
		if code != 200 || body["status"] != tc.status || printer.jobs != tc.jobs {
			t.Errorf("scan %s: HTTP %d, status %v, %d jobs; want %s, %d jobs", tc.id, code, body["status"], printer.jobs, tc.status, tc.jobs)
		}
	}

	_, body := post("/scan/A1", "")
	guest := body["ticket"].(map[string]any)
	if guest["company"] != "Acme" || guest["ticket_group"] != "Room A" || guest["custom"].(map[string]any)["custom_tier"] != "Gold" {
		t.Errorf("ticket mapping: %v", guest)
	}
	if _, body := post("/scan/DUPE", ""); body["checked_in_at"] != "2026-11-17T09:00:00Z" || body["checked_in_by"] != "Gate 2" {
		t.Errorf("earlier scan details missing: %v", body)
	}
	// Unpaid is a refusal, not "already": it must not be offered a reprint.
	if code, body := post("/scan/UNPAID", ""); code != 502 || body["detail"] != "Ticket is not paid" {
		t.Errorf("unpaid: HTTP %d %v", code, body)
	}
	// A printer fault after check-in is an outcome, not a failed scan.
	printer.fault = errors.New("out of paper")
	if code, body := post("/scan/A1", ""); code != 200 || body["status"] != statusPrintFailed {
		t.Errorf("printer fault: HTTP %d %v", code, body)
	}
	printer.fault, printer.jobs = nil, 0
	// Reprint ignores the skip list, and a local origin is allowed.
	if code, body := post("/scan/STAFF/reprint", "http://127.0.0.1:8000"); code != 200 || body["reprinted"] != true || printer.jobs != 1 {
		t.Errorf("reprint: HTTP %d %v", code, body)
	}
	if code, _ := post("/scan/A1", "https://evil.example"); code != 403 {
		t.Errorf("cross-site POST allowed: HTTP %d", code)
	}
}

func TestRebindingHostIsRefused(t *testing.T) {
	app := &App{Store: config.Open(filepath.Join(t.TempDir(), "config.json")), Printer: &fakePrinter{}, Updater: update.New("dev", "")}
	for host, want := range map[string]int{"127.0.0.1:8000": 200, "localhost:8000": 200, "[::1]:8000": 200, "attacker.example:8000": 403} {
		req := httptest.NewRequest("GET", "/config", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: HTTP %d, want %d", host, rec.Code, want)
		}
	}
	app.OnNetwork = true
	req := httptest.NewRequest("GET", "/config", nil)
	req.Host = "192.168.1.20:8000"
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("network mode refused a LAN host: HTTP %d", rec.Code)
	}
}

func TestSearchMethod(t *testing.T) {
	for query, want := range map[string]string{
		"a@b.co": "email", "a@b": "", "+60 12-345": "phone", "123": "", "Li": "name", "L": "", "Room 12": "name",
	} {
		if got := searchMethod(query); got != want {
			t.Errorf("searchMethod(%q) = %q, want %q", query, got, want)
		}
	}
}
