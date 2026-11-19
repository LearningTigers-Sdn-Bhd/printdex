// Package config holds a station's settings — the event connection and the
// badge layout — and the file they are kept in.
//
// A station upgrading from the earlier event-printer app keeps its settings:
// that app's config file is adopted on first start.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// CustomPrefix starts the id of every user-defined badge element.
const CustomPrefix = "custom_"

const (
	maxCustomFields = 6
	maxTypeNames    = 30
	maxPresets      = 20
)

// builtinElements are the badge elements a layout can place, besides custom fields.
var builtinElements = []string{"name", "role", "ticket_role", "ticket_group", "company", "title", "country", "table_no", "qr"}

// BoldByDefault reports whether an element prints bold unless the layout
// says otherwise: the headline elements do.
func BoldByDefault(element string) bool {
	return element == "name" || element == "role" || element == "table_no"
}

type Paper struct {
	WidthMM  float64 `json:"width_mm"`
	HeightMM float64 `json:"height_mm"`
}

// Offset nudges one element from where the layout would put it.
type Offset struct {
	DX float64 `json:"dx_mm"` // positive = right
	DY float64 `json:"dy_mm"` // positive = up
}

// CustomField is a user-defined badge element fed from the event's custom
// registration fields.
type CustomField struct {
	Label      string `json:"label"`
	BackendKey string `json:"backend_key"` // key in the ticket's custom fields; empty = match by label
}

// Layout describes a badge: the paper and the elements on it, top to bottom.
// The maps hold only departures from the defaults.
type Layout struct {
	Paper            Paper                  `json:"paper"`
	Elements         []string               `json:"elements"`
	CustomFields     map[string]CustomField `json:"custom_fields"`
	ElementScales    map[string]float64     `json:"element_scales"`  // size multiplier, 1 = auto
	ElementBolds     map[string]bool        `json:"element_bolds"`   // weight override
	ElementOffsets   map[string]Offset      `json:"element_offsets"` // position nudge
	VerticalOffsetMM float64                `json:"vertical_offset_mm"`
	// QRStatic is what the QR prints instead of the ticket id; "{id}" inside it
	// becomes the ticket id. Empty = the bare ticket id.
	QRStatic string `json:"qr_static"`
	// QRLabel is a small caption printed under the QR code. Empty = none.
	QRLabel string `json:"qr_label"`
}

func DefaultLayout() Layout {
	return Layout{
		Paper:          Paper{WidthMM: 100, HeightMM: 80},
		Elements:       []string{"name", "role", "company", "qr"},
		CustomFields:   map[string]CustomField{},
		ElementScales:  map[string]float64{},
		ElementBolds:   map[string]bool{},
		ElementOffsets: map[string]Offset{},
	}
}

// Normalize returns the layout with unknown elements dropped, values clamped
// to their ranges and defaults removed. It fails if no usable badge is left.
func (l Layout) Normalize() (Layout, error) {
	for _, mm := range []float64{l.Paper.WidthMM, l.Paper.HeightMM} {
		if !(mm >= 20 && mm <= 500) {
			return Layout{}, errors.New("paper width and height must be 20-500 mm")
		}
	}
	out := DefaultLayout()
	out.Paper = l.Paper
	out.Elements = nil

	for _, id := range sortedKeys(l.CustomFields) {
		field := l.CustomFields[id]
		label := clip(strings.TrimSpace(field.Label), 60)
		if !strings.HasPrefix(id, CustomPrefix) || label == "" || len(out.CustomFields) >= maxCustomFields {
			continue
		}
		out.CustomFields[clip(id, len(CustomPrefix)+12)] = CustomField{Label: label, BackendKey: clip(strings.TrimSpace(field.BackendKey), 60)}
	}
	known := func(element string) bool {
		_, custom := out.CustomFields[element]
		return custom || slices.Contains(builtinElements, element)
	}
	for _, element := range l.Elements {
		if known(element) && !slices.Contains(out.Elements, element) {
			out.Elements = append(out.Elements, element)
		}
	}
	if len(out.Elements) == 0 {
		return Layout{}, errors.New("a layout needs at least one element")
	}
	for element, scale := range l.ElementScales {
		if known(element) && math.Abs(scale-1) > 1e-9 {
			out.ElementScales[element] = roundTo(clamp(scale, 0.5, 2), 2)
		}
	}
	for element, bold := range l.ElementBolds {
		if known(element) && bold != BoldByDefault(element) {
			out.ElementBolds[element] = bold
		}
	}
	for element, offset := range l.ElementOffsets {
		offset = Offset{DX: roundTo(clamp(offset.DX, -10, 10), 1), DY: roundTo(clamp(offset.DY, -10, 10), 1)}
		if known(element) && offset != (Offset{}) {
			out.ElementOffsets[element] = offset
		}
	}
	out.VerticalOffsetMM = roundTo(clamp(l.VerticalOffsetMM, -80, 80), 1)
	out.QRStatic = clip(strings.TrimSpace(l.QRStatic), 500)
	out.QRLabel = clip(strings.TrimSpace(l.QRLabel), 40)
	return out, nil
}

// Config is everything a station remembers.
type Config struct {
	BackendURL         string            `json:"backend_url"`
	EventSlug          string            `json:"event_slug"`
	APIKey             string            `json:"api_key"`
	BadgeTypes         []string          `json:"badge_types"` // choices offered for manual entry
	Layout             Layout            `json:"layout"`
	LayoutPresets      map[string]Layout `json:"layout_presets"`
	ActivePreset       *string           `json:"active_preset"`
	ShowScanner        bool              `json:"show_scanner"`
	DirectThermal      bool              `json:"direct_thermal"`        // thicken strokes for direct-thermal stock
	NoPrintTicketTypes []string          `json:"no_print_ticket_types"` // check in without printing a badge
}

// Default is a fresh station. The event connection can be preset through
// EVENTZ_BACKEND_URL, EVENTZ_EVENT_SLUG and EVENTZ_API_KEY.
func Default() Config {
	return Config{
		BackendURL:         os.Getenv("EVENTZ_BACKEND_URL"),
		EventSlug:          os.Getenv("EVENTZ_EVENT_SLUG"),
		APIKey:             os.Getenv("EVENTZ_API_KEY"),
		BadgeTypes:         []string{"Visitor", "Delegate", "Speaker", "Moderator", "Student", "Staff", "Organizer", "Exhibitor", "VIP", "VVIP"},
		Layout:             DefaultLayout(),
		LayoutPresets:      map[string]Layout{},
		ShowScanner:        true,
		NoPrintTicketTypes: []string{},
	}
}

// Connected reports whether the station knows which event it serves.
func (c Config) Connected() bool { return c.BackendURL != "" && c.EventSlug != "" }

// SkipsBadge reports whether tickets of this type check in without a badge.
func (c Config) SkipsBadge(ticketType string) bool {
	return slices.ContainsFunc(c.NoPrintTicketTypes, func(t string) bool {
		return strings.EqualFold(t, strings.TrimSpace(ticketType))
	})
}

// Public is the config as the dashboard sees it: the API key is masked.
type Public struct {
	BackendURL         string            `json:"backend_url"`
	EventSlug          string            `json:"event_slug"`
	APIKeyMasked       string            `json:"api_key_masked"`
	APIKeySet          bool              `json:"api_key_set"`
	ConfigPath         string            `json:"config_path"`
	BadgeTypes         []string          `json:"badge_types"`
	Layout             Layout            `json:"layout"`
	LayoutPresets      map[string]Layout `json:"layout_presets"`
	ActivePreset       *string           `json:"active_preset"`
	ShowScanner        bool              `json:"show_scanner"`
	DirectThermal      bool              `json:"direct_thermal"`
	NoPrintTicketTypes []string          `json:"no_print_ticket_types"`
}

func (c Config) Public(path string) Public {
	key := []rune(c.APIKey)
	shown := min(4, max(0, len(key)-4)) // never reveal a key too short to hide
	return Public{
		BackendURL: c.BackendURL, EventSlug: c.EventSlug,
		APIKeyMasked: strings.Repeat("•", len(key)-shown) + string(key[len(key)-shown:]),
		APIKeySet:    c.APIKey != "", ConfigPath: path,
		BadgeTypes: c.BadgeTypes, Layout: c.Layout, LayoutPresets: c.LayoutPresets, ActivePreset: c.ActivePreset,
		ShowScanner: c.ShowScanner, DirectThermal: c.DirectThermal, NoPrintTicketTypes: c.NoPrintTicketTypes,
	}
}

// Patch is a partial update: nil fields are left as they are. It is also the
// shape of the stored file.
type Patch struct {
	BackendURL         *string           `json:"backend_url"`
	EventSlug          *string           `json:"event_slug"`
	APIKey             *string           `json:"api_key"`
	BadgeTypes         []string          `json:"badge_types"`
	Layout             *Layout           `json:"layout"`
	LayoutPresets      map[string]Layout `json:"layout_presets"`
	ActivePreset       *string           `json:"active_preset"`
	ShowScanner        *bool             `json:"show_scanner"`
	DirectThermal      *bool             `json:"direct_thermal"`
	NoPrintTicketTypes []string          `json:"no_print_ticket_types"`
}

// apply merges p into c. Anything unusable in p is skipped, so one bad value
// never costs the station the rest of its settings.
func (c *Config) apply(p Patch) {
	// The API key belongs to one backend. Pointing the station at another
	// address without supplying a key drops the old one, so it can never be
	// sent somewhere the operator didn't enter it for.
	if p.BackendURL != nil && p.APIKey == nil && strings.TrimSpace(*p.BackendURL) != c.BackendURL {
		c.APIKey = ""
	}
	for _, field := range []struct{ dst, src *string }{
		{&c.BackendURL, p.BackendURL}, {&c.EventSlug, p.EventSlug}, {&c.APIKey, p.APIKey},
	} {
		if field.src != nil {
			*field.dst = strings.TrimSpace(*field.src)
		}
	}
	if p.ShowScanner != nil {
		c.ShowScanner = *p.ShowScanner
	}
	if p.DirectThermal != nil {
		c.DirectThermal = *p.DirectThermal
	}
	if types := cleanNames(p.BadgeTypes); len(types) > 0 { // the list may not be emptied
		c.BadgeTypes = types
	}
	if p.NoPrintTicketTypes != nil {
		c.NoPrintTicketTypes = cleanNames(p.NoPrintTicketTypes)
	}
	if p.Layout != nil {
		if layout, err := p.Layout.Normalize(); err == nil {
			c.Layout = layout
		}
	}
	if p.LayoutPresets != nil {
		c.LayoutPresets = map[string]Layout{}
		for _, name := range sortedKeys(p.LayoutPresets) {
			layout, err := p.LayoutPresets[name].Normalize()
			if name = clip(strings.TrimSpace(name), 40); err == nil && name != "" && len(c.LayoutPresets) < maxPresets {
				c.LayoutPresets[name] = layout
			}
		}
	}
	if p.ActivePreset != nil {
		c.ActivePreset = p.ActivePreset
	}
	if c.ActivePreset != nil {
		if _, exists := c.LayoutPresets[*c.ActivePreset]; !exists {
			c.ActivePreset = nil
		}
	}
}

// Store keeps the config in memory and writes it to disk on every change.
type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

// DefaultPath is the config file in the user's settings folder.
func DefaultPath() string { return userFile("printdex") }

// LegacyPath is where the earlier event-printer app kept its config.
func LegacyPath() string { return userFile("event-printer") }

func userFile(app string) string {
	dir, err := os.UserConfigDir() // %APPDATA%, ~/Library/Application Support, ~/.config
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, app, "config.json")
}

// Open loads the config from path. If that file doesn't exist yet, settings
// are adopted from the first fallback that does; saves always go to path.
func Open(path string, fallbacks ...string) *Store {
	s := &Store{path: path, cfg: Default()}
	for _, file := range append([]string{path}, fallbacks...) {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var stored Patch
		if json.Unmarshal(data, &stored) == nil {
			s.cfg.apply(stored)
		}
		break
	}
	return s
}

func (s *Store) Path() string { return s.path }

// Get returns the current config. Treat its maps and slices as read-only.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Save applies p and writes the result. An invalid layout is rejected; the
// file is replaced atomically and readable by its owner only (it holds the
// API key).
func (s *Store) Save(p Patch) (Config, error) {
	if p.Layout != nil {
		if _, err := p.Layout.Normalize(); err != nil {
			return Config{}, fmt.Errorf("layout: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	next.apply(p)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return Config{}, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return Config{}, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return Config{}, err
	}
	s.cfg = next
	return next, nil
}

// cleanNames trims a list of type names, drops blanks and case-insensitive
// repeats, and caps its length.
func cleanNames(list []string) []string {
	out := []string{}
	for _, name := range list {
		name = clip(strings.TrimSpace(name), 40)
		repeat := slices.ContainsFunc(out, func(seen string) bool { return strings.EqualFold(seen, name) })
		if name != "" && !repeat && len(out) < maxTypeNames {
			out = append(out, name)
		}
	}
	return out
}

func clamp(v, lo, hi float64) float64 { return math.Min(hi, math.Max(lo, v)) }

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// clip shortens s to at most n characters.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// sortedKeys gives map iteration a stable order, so caps keep the same entries every time.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
