package ticket

import "testing"

func TestParseReadsLooselyTypedFields(t *testing.T) {
	got, err := Parse([]byte(`{
		"ticket_id": " A1-0245 ", "ticket_type": "Delegate", "name": "Aisha Tan",
		"organisation": "Acme", "table_number": 12,
		"ticket_group": {"id": 3, "name": "Room A"}, "role": null, "title": ["not", "text"],
		"custom": {"custom_ab": {"label": "Gold"}, "custom_cd": "  "}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "A1-0245" || got.Company != "Acme" || got.TableNo != "12" || got.TicketGroup != "Room A" {
		t.Errorf("got %+v", got)
	}
	if got.Role != "" || got.Title != "" {
		t.Errorf("null and non-text values should read as empty: role %q, title %q", got.Role, got.Title)
	}
	if len(got.Custom) != 1 || got.Custom["custom_ab"] != "Gold" {
		t.Errorf("custom = %v", got.Custom)
	}
}

func TestParseRejectsUnprintableTickets(t *testing.T) {
	for name, body := range map[string]string{
		"no id":    `{"ticket_type": "Delegate"}`,
		"no type":  `{"ticket_id": "A1"}`,
		"not json": `nope`,
		"a list":   `[]`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
