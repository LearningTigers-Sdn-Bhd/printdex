// Package ticket defines what a badge can show about a guest.
package ticket

import (
	"encoding/json"
	"errors"
	"strings"
)

// Ticket is one guest's printable details. Empty fields are simply left off
// the badge.
type Ticket struct {
	ID          string            `json:"ticket_id"`
	Name        string            `json:"name"`
	Company     string            `json:"company"`
	Title       string            `json:"title"`
	Type        string            `json:"ticket_type"`
	Role        string            `json:"role"`
	TicketGroup string            `json:"ticket_group"`
	Country     string            `json:"country"`
	TableNo     string            `json:"table_no"`
	Custom      map[string]string `json:"custom"` // layout custom-field id -> text
}

// Text is a string read leniently from JSON. Event data is loosely typed: a
// field may arrive as a string, a number, or an object such as
// {"id": 3, "name": "Room A"}. Anything else reads as empty.
type Text string

// labelKeys are tried in order when a value arrives as an object.
var labelKeys = []string{"name", "label", "value", "title", "company", "organisation_institution"}

func (t *Text) UnmarshalJSON(data []byte) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*t = ""
	switch v := value.(type) {
	case string:
		*t = Text(v)
	case float64:
		*t = Text(strings.TrimSpace(string(data))) // the number as written
	case map[string]any:
		for _, key := range labelKeys {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				*t = Text(s)
				break
			}
		}
	}
	return nil
}

// First returns the first non-blank value.
func First(values ...Text) string {
	for _, v := range values {
		if strings.TrimSpace(string(v)) != "" {
			return string(v)
		}
	}
	return ""
}

// Parse reads a ticket sent by the dashboard or another local app. It accepts
// the alternate field names older callers use.
func Parse(data []byte) (Ticket, error) {
	var in struct {
		ID           Text            `json:"ticket_id"`
		Name         Text            `json:"name"`
		Company      Text            `json:"company"`
		Organisation Text            `json:"organisation"`
		Organization Text            `json:"organization"`
		Title        Text            `json:"title"`
		Type         Text            `json:"ticket_type"`
		Role         Text            `json:"role"`
		TicketGroup  Text            `json:"ticket_group"`
		Country      Text            `json:"country"`
		TableNo      Text            `json:"table_no"`
		TableNumber  Text            `json:"table_number"`
		Table        Text            `json:"table"`
		Custom       map[string]Text `json:"custom"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return Ticket{}, errors.New("ticket must be a JSON object")
	}
	t := Ticket{
		ID: string(in.ID), Name: string(in.Name), Title: string(in.Title), Type: string(in.Type),
		Role: string(in.Role), TicketGroup: string(in.TicketGroup), Country: string(in.Country),
		Company: First(in.Company, in.Organisation, in.Organization),
		TableNo: First(in.TableNo, in.TableNumber, in.Table),
		Custom:  map[string]string{},
	}
	for key, value := range in.Custom {
		t.Custom[key] = string(value)
	}
	return t.Clean()
}

// Clean trims every field to a sane length and checks the ticket can be
// printed: it needs an id (the QR code) and a type.
func (t Ticket) Clean() (Ticket, error) {
	for _, field := range []struct {
		value *string
		max   int
	}{
		{&t.ID, 100}, {&t.Type, 100}, {&t.Name, 200}, {&t.Company, 200}, {&t.Title, 200},
		{&t.Role, 100}, {&t.TicketGroup, 100}, {&t.Country, 100}, {&t.TableNo, 50},
	} {
		*field.value = tidy(*field.value, field.max)
	}
	custom := map[string]string{}
	for key, value := range t.Custom {
		if key, value = tidy(key, 60), tidy(value, 200); key != "" && value != "" {
			custom[key] = value
		}
	}
	t.Custom = custom
	switch {
	case t.ID == "":
		return t, errors.New("ticket_id is required")
	case t.Type == "":
		return t, errors.New("ticket_type is required")
	}
	return t, nil
}

func tidy(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
