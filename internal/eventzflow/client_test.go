package eventzflow

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/ticket"
)

// serve starts a fake backend answering every request with status and body.
func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return New(config.Config{BackendURL: server.URL + "/", EventSlug: "expo", APIKey: "key"})
}

func TestTicketMapsCustomFieldsByKeyThenLabel(t *testing.T) {
	data := TicketData{
		PublicID: "A1", AttendeeName: "Aisha Tan",
		CustomFields: map[string]ticket.Text{"organisation_institution": "Acme", "Sponsor Tier": "Gold", "meal": "Veg"},
	}
	got, err := data.Ticket(map[string]config.CustomField{
		"custom_a": {Label: "x", BackendKey: "meal"}, // by key
		"custom_b": {Label: "sponsor tier"},          // by label, any case
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Company != "Acme" || got.Type != "Visitor" || got.Custom["custom_a"] != "Veg" || got.Custom["custom_b"] != "Gold" {
		t.Errorf("got %+v", got)
	}
}

func TestCheckInTellsAlreadyScannedFromRefused(t *testing.T) {
	var already *AlreadyCheckedIn
	_, err := serve(t, 422, `{"error":"Ticket already scanned","blocked_by":{"scanned_by_name":"Gate 2","location_name":"Main"}}`).CheckIn("A1")
	if !errors.As(err, &already) || already.By != "Gate 2" || already.Location != "Main" {
		t.Errorf("already-scanned reply: %#v", err)
	}
	_, err = serve(t, 422, `{"error":"Ticket is not paid"}`).CheckIn("A1")
	if errors.As(err, &already) || err == nil || err.Error() != "Ticket is not paid" {
		t.Errorf("unpaid must be a plain refusal, got %#v", err)
	}
}

func TestSearchPassesGuestsThroughAndTreatsMissingAsEmpty(t *testing.T) {
	guests, err := serve(t, 200, `{"data":{"attendees":[{"public_id":"A1","name":"Li"},{"name":"no id"}]}}`).Search("name", "Li")
	if err != nil || len(guests) != 1 || !strings.Contains(string(guests[0]), "A1") {
		t.Errorf("guests = %v, err = %v", guests, err)
	}
	if guests, err := serve(t, 404, `{"message":"No attendees found"}`).Search("name", "Zz"); err != nil || len(guests) != 0 {
		t.Errorf("no match should be an empty list: %v, %v", guests, err)
	}
	if _, err := serve(t, 404, `{"message":"Event not found"}`).Search("name", "Li"); err == nil {
		t.Error("unknown event should be an error")
	}
}

func TestTicketGroupsDisabledIsNotAnError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/check_in") {
			w.Write([]byte(`{"data":{"id":7}}`))
			return
		}
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"Ticket groups not enabled"}`))
	}))
	defer server.Close()
	enabled, groups, err := New(config.Config{BackendURL: server.URL, EventSlug: "expo", APIKey: "key"}).TicketGroups()
	if err != nil || enabled || len(groups) != 0 || calls != 2 {
		t.Errorf("enabled=%v groups=%v err=%v calls=%d", enabled, groups, err, calls)
	}
}
