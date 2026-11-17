// Package eventzflow talks to the EventzFlow backend: ticket lookup, check-in
// and guest search.
package eventzflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/ticket"
)

// One shared connection pool: the TLS session to the backend stays open
// between scans, so a scan costs two round trips and no handshakes.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     10 * time.Minute,
	},
}

// Error is a failure worded for the operator.
type Error string

func (e Error) Error() string { return string(e) }

// AlreadyCheckedIn is returned by CheckIn when the ticket was scanned before.
// The details are filled in when the backend reports them.
type AlreadyCheckedIn struct {
	Message  string
	At       string
	By       string
	Location string
}

func (e *AlreadyCheckedIn) Error() string { return e.Message }

// errEventNotFound is what every lookup says when the backend doesn't know the
// event the station was given.
var errEventNotFound = Error("Event not found. Check the event ID or slug.")

// Client reaches one event on one backend. The event is named by its numeric ID
// or its slug, whichever the operator entered: the backend accepts both in the
// same place of the address.
type Client struct {
	baseURL string
	slug    string
	apiKey  string
}

func New(cfg config.Config) *Client {
	return &Client{baseURL: strings.TrimRight(cfg.BackendURL, "/"), slug: cfg.EventSlug, apiKey: strings.TrimSpace(cfg.APIKey)}
}

// response is a backend reply: its status and raw body.
type response struct {
	status int
	body   []byte
}

// decode reads the body as JSON into v; false if it isn't that shape.
func (r response) decode(v any) bool { return json.Unmarshal(r.body, v) == nil }

// excerpt is a short single-line quote of the body for error messages.
func (r response) excerpt() string {
	text := []rune(strings.ReplaceAll(strings.TrimSpace(string(r.body)), "\n", " "))
	if len(text) > 200 {
		return string(text[:200]) + "…"
	}
	return string(text)
}

func (c *Client) send(method, path string, payload any, withKey bool) (response, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return response{}, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return response{}, Error("The backend address is not valid.")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withKey {
		req.Header.Set("Authorization", c.apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{}, Error("Network error contacting backend: " + err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return response{}, Error("Network error contacting backend: " + err.Error())
	}
	return response{status: resp.StatusCode, body: data}, nil
}

func (c *Client) eventPath(suffix string) string {
	return "/v1/public/events/" + url.PathEscape(c.slug) + suffix
}

// EventInfo identifies an event.
type EventInfo struct {
	ID    ticket.Text `json:"id"`
	Title ticket.Text `json:"title"`
	Slug  ticket.Text `json:"slug"`
}

// Event looks up the connected event, which shows the operator what the ID or
// slug they entered points to.
func (c *Client) Event() (EventInfo, error) {
	resp, err := c.send("GET", c.eventPath("/check_in"), nil, false)
	switch {
	case err != nil:
		return EventInfo{}, err
	case resp.status == 404:
		return EventInfo{}, errEventNotFound
	case resp.status >= 400:
		return EventInfo{}, Error(fmt.Sprintf("Event lookup failed (%d): %s", resp.status, resp.excerpt()))
	}
	var reply struct {
		Data EventInfo `json:"data"`
	}
	if !resp.decode(&reply) || reply.Data.ID == "" {
		return EventInfo{}, Error("The backend sent event details this app can't read.")
	}
	return reply.Data, nil
}

// TicketData is a ticket as the backend describes it.
type TicketData struct {
	PublicID     ticket.Text            `json:"public_id"`
	AttendeeName ticket.Text            `json:"attendee_name"`
	TicketType   ticket.Text            `json:"ticket_type"`
	Role         ticket.Text            `json:"role"`
	TicketGroup  ticket.Text            `json:"ticket_group"` // {id, name} or null
	CustomFields map[string]ticket.Text `json:"custom_fields_data"`
}

// Ticket maps backend data onto a printable ticket. Company, title, country
// and table come from the event's custom registration fields, whose keys vary
// by event; customFields are the layout's own custom elements.
func (d TicketData) Ticket(customFields map[string]config.CustomField) (ticket.Ticket, error) {
	field := func(keys ...string) string {
		values := make([]ticket.Text, len(keys))
		for i, key := range keys {
			values[i] = d.CustomFields[key]
		}
		return ticket.First(values...)
	}
	byLabel := map[string]ticket.Text{}
	for key, value := range d.CustomFields {
		byLabel[strings.ToLower(strings.TrimSpace(key))] = value
	}
	custom := map[string]string{}
	for id, def := range customFields {
		custom[id] = ticket.First(d.CustomFields[def.BackendKey], byLabel[strings.ToLower(def.Label)])
	}
	return ticket.Ticket{
		ID:          string(d.PublicID),
		Name:        string(d.AttendeeName),
		Type:        ticket.First(d.TicketType, "Visitor"),
		Role:        string(d.Role),
		TicketGroup: string(d.TicketGroup),
		Company:     field("company", "organisation_institution", "organization", "organisation"),
		Title:       field("title", "position", "job_title", "designation"),
		Country:     field("country"),
		TableNo:     field("table_no", "table_number", "table"),
		Custom:      custom,
	}.Clean()
}

// FetchTicket looks a ticket up by its public id (the QR code's content).
func (c *Client) FetchTicket(publicID string) (TicketData, error) {
	resp, err := c.send("GET", c.eventPath("/tickets/"+url.PathEscape(publicID)), nil, false)
	switch {
	case err != nil:
		return TicketData{}, err
	case resp.status == 404:
		return TicketData{}, Error("Ticket not found for this event.")
	case resp.status == 401 || resp.status == 403:
		return TicketData{}, Error("Not authorized. Check your API key.")
	case resp.status >= 400:
		return TicketData{}, Error(fmt.Sprintf("Lookup failed (%d): %s", resp.status, resp.excerpt()))
	}
	var reply struct {
		Data *TicketData `json:"data"`
	}
	if !resp.decode(&reply) || reply.Data == nil {
		return TicketData{}, Error("The backend sent a ticket this app can't read.")
	}
	return *reply.Data, nil
}

type named struct {
	Name ticket.Text `json:"name"`
}

// distinctNames lists the non-blank names once each, in order.
func distinctNames(rows []named) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, row := range rows {
		if name := strings.TrimSpace(string(row.Name)); name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// TicketTypes lists the event's publicly listed ticket types. Hidden ones
// (staff, comps) aren't included.
func (c *Client) TicketTypes() ([]string, error) {
	resp, err := c.send("GET", c.eventPath("/ticket_types"), nil, false)
	switch {
	case err != nil:
		return nil, err
	case resp.status == 404:
		return nil, errEventNotFound
	case resp.status >= 400:
		return nil, Error(fmt.Sprintf("Ticket type lookup failed (%d): %s", resp.status, resp.excerpt()))
	}
	var reply struct {
		Data []named `json:"data"`
	}
	if !resp.decode(&reply) {
		return nil, Error("Ticket type lookup returned an unexpected response.")
	}
	return distinctNames(reply.Data), nil
}

// TicketGroups lists the event's ticket groups. enabled is false when the
// event doesn't use groups.
func (c *Client) TicketGroups() (enabled bool, groups []string, err error) {
	if c.apiKey == "" {
		return false, nil, Error("API key is not configured.")
	}
	// The group list is keyed by the numeric event id, which the event's
	// public info provides.
	event, err := c.Event()
	if err != nil {
		return false, nil, err
	}
	resp, err := c.send("GET", "/v1/events/"+url.PathEscape(string(event.ID))+"/ticket_groups", nil, true)
	if err != nil {
		return false, nil, err
	}
	var reply struct {
		Error  ticket.Text `json:"error"`
		Groups []named     `json:"groups"`
	}
	resp.decode(&reply)
	switch {
	case resp.status == 403 && strings.Contains(strings.ToLower(string(reply.Error)), "not enabled"):
		return false, []string{}, nil // the backend's way of saying "no groups"
	case resp.status == 401 || resp.status == 403:
		return false, nil, Error("Not authorized to read ticket groups. Check your API key.")
	case resp.status >= 400:
		return false, nil, Error(fmt.Sprintf("Group lookup failed (%d): %s", resp.status, resp.excerpt()))
	}
	return true, distinctNames(reply.Groups), nil
}

// Search finds guests by "name", "email" or "phone" — the same no-login
// search the EventzFlow check-in page uses. Nothing is checked in. Results
// are passed through as the backend sent them.
func (c *Client) Search(method, value string) ([]json.RawMessage, error) {
	resp, err := c.send("POST", c.eventPath("/check_in"), map[string]string{"method": method, "value": value}, false)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Message string `json:"message"`
		Data    struct {
			Attendees []json.RawMessage `json:"attendees"`
		} `json:"data"`
	}
	resp.decode(&reply)
	switch {
	case resp.status == 404 && strings.Contains(strings.ToLower(reply.Message), "event"):
		return nil, errEventNotFound
	case resp.status == 404:
		return []json.RawMessage{}, nil // no guest matches
	case resp.status >= 400:
		detail := reply.Message
		if detail == "" {
			detail = resp.excerpt()
		}
		return nil, Error(fmt.Sprintf("Search failed (%d): %s", resp.status, detail))
	}
	guests := []json.RawMessage{}
	for _, row := range reply.Data.Attendees {
		var guest struct {
			PublicID ticket.Text `json:"public_id"`
		}
		if json.Unmarshal(row, &guest) == nil && guest.PublicID != "" {
			guests = append(guests, row)
		}
	}
	return guests, nil
}

// CheckIn marks the ticket as scanned and returns the backend's reply. It
// returns *AlreadyCheckedIn if the ticket was scanned before.
func (c *Client) CheckIn(publicID string) (json.RawMessage, error) {
	if c.apiKey == "" {
		return nil, Error("API key is not configured. Cannot mark ticket as scanned.")
	}
	resp, err := c.send("PATCH", "/v1/scan/"+url.PathEscape(publicID)+"/check_in", nil, true)
	switch {
	case err != nil:
		return nil, err
	case resp.status == 422:
		return nil, refusal(resp)
	case resp.status == 401 || resp.status == 403:
		return nil, Error("Not authorized. Check your API key permissions.")
	case resp.status == 404:
		return nil, Error("Ticket not found while attempting check-in.")
	case resp.status >= 400:
		return nil, Error(fmt.Sprintf("Check-in failed (%d): %s", resp.status, resp.excerpt()))
	}
	if !json.Valid(resp.body) {
		return json.RawMessage("{}"), nil
	}
	return resp.body, nil
}

// refusal reads a 422 from check-in. The backend uses it both for tickets
// already scanned and for ones it won't admit (unpaid). Only the first may
// be offered a reprint, so the two must not be confused.
func refusal(resp response) error {
	var reply struct {
		Error       ticket.Text `json:"error"`
		Message     ticket.Text `json:"message"`
		CheckedInAt *string     `json:"checked_in_at"`
		BlockedBy   *struct {
			ScannedAt string `json:"scanned_at"`
			ScannedBy string `json:"scanned_by_name"`
			Location  string `json:"location_name"`
		} `json:"blocked_by"`
	}
	if !resp.decode(&reply) {
		return &AlreadyCheckedIn{Message: "Ticket already checked in."}
	}
	message := ticket.First(reply.Error, reply.Message, "Ticket already checked in.")
	scannedBefore := reply.CheckedInAt != nil || reply.BlockedBy != nil ||
		strings.Contains(strings.ToLower(message), "already")
	if !scannedBefore {
		return Error(message)
	}
	already := &AlreadyCheckedIn{Message: message}
	if reply.BlockedBy != nil {
		already.At, already.By, already.Location = reply.BlockedBy.ScannedAt, reply.BlockedBy.ScannedBy, reply.BlockedBy.Location
	}
	if reply.CheckedInAt != nil && *reply.CheckedInAt != "" {
		already.At = *reply.CheckedInAt
	}
	return already
}

// Warm opens the connection to the backend in the background, so the first
// scan doesn't pay for the handshake.
func (c *Client) Warm() {
	go func() { _, _ = c.TicketTypes() }()
}
