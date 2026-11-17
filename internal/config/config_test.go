package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDropsJunkAndClamps(t *testing.T) {
	got, err := Layout{
		Paper:            Paper{WidthMM: 90, HeightMM: 60},
		Elements:         []string{"name", "bogus", "qr", "name", "custom_ab"},
		CustomFields:     map[string]CustomField{"custom_ab": {Label: " Sponsor "}, "nope": {Label: "x"}, "custom_cd": {}},
		ElementScales:    map[string]float64{"name": 9, "qr": 1, "bogus": 2},
		ElementBolds:     map[string]bool{"name": true, "company": true},
		ElementOffsets:   map[string]Offset{"qr": {DX: 99}, "name": {}},
		VerticalOffsetMM: -500,
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Elements, ",") != "name,qr,custom_ab" {
		t.Errorf("elements = %v", got.Elements)
	}
	if len(got.CustomFields) != 1 || got.CustomFields["custom_ab"].Label != "Sponsor" {
		t.Errorf("custom fields = %v", got.CustomFields)
	}
	if len(got.ElementScales) != 1 || got.ElementScales["name"] != 2 {
		t.Errorf("scales = %v (want name clamped to 2, defaults dropped)", got.ElementScales)
	}
	if len(got.ElementBolds) != 1 || !got.ElementBolds["company"] {
		t.Errorf("bolds = %v (want only the non-default one)", got.ElementBolds)
	}
	if len(got.ElementOffsets) != 1 || got.ElementOffsets["qr"].DX != 10 || got.VerticalOffsetMM != -80 {
		t.Errorf("offsets = %v, vertical %v", got.ElementOffsets, got.VerticalOffsetMM)
	}

	for name, bad := range map[string]Layout{
		"tiny paper":  {Paper: Paper{WidthMM: 5, HeightMM: 60}, Elements: []string{"name"}},
		"no elements": {Paper: Paper{WidthMM: 90, HeightMM: 60}, Elements: []string{"bogus"}},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStoreAdoptsLegacyFileThenSavesItsOwn(t *testing.T) {
	dir := t.TempDir()
	own, legacy := filepath.Join(dir, "printdex", "config.json"), filepath.Join(dir, "legacy.json")
	os.WriteFile(legacy, []byte(`{"backend_url": " https://api.example.test ", "event_slug": "expo",
		"api_key": "secret-key-1234", "no_print_ticket_types": ["Staff", "staff", ""],
		"layout": {"paper": {"width_mm": 90, "height_mm": 60}, "elements": ["name", "qr"]}}`), 0o600)

	store := Open(own, legacy)
	cfg := store.Get()
	if cfg.BackendURL != "https://api.example.test" || !cfg.Connected() || cfg.Layout.Paper.WidthMM != 90 {
		t.Errorf("legacy settings not adopted: %+v", cfg)
	}
	if !cfg.SkipsBadge(" STAFF ") || cfg.SkipsBadge("Delegate") || len(cfg.NoPrintTicketTypes) != 1 {
		t.Errorf("skip list = %v", cfg.NoPrintTicketTypes)
	}
	if masked := cfg.Public(own).APIKeyMasked; masked != strings.Repeat("•", 11)+"1234" {
		t.Errorf("masked key = %q", masked)
	}

	slug := "summit"
	if _, err := store.Save(Patch{EventSlug: &slug}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(Patch{Layout: &Layout{}}); err == nil {
		t.Error("invalid layout was saved")
	}
	reopened := Open(own, legacy).Get()
	if reopened.EventSlug != "summit" || reopened.APIKey != "secret-key-1234" {
		t.Errorf("save/reopen lost data: %+v", reopened)
	}
	if info, _ := os.Stat(own); info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %v, want owner-only (it holds the API key)", info.Mode().Perm())
	}
}

func TestChangingBackendDropsTheOldAPIKey(t *testing.T) {
	url, same, other, key := "https://a.example", "https://a.example", "https://evil.example", "k-123456"
	store := Open(filepath.Join(t.TempDir(), "config.json"))
	store.Save(Patch{BackendURL: &url, APIKey: &key})
	if cfg, _ := store.Save(Patch{BackendURL: &same}); cfg.APIKey != key {
		t.Error("re-saving the same address dropped the key")
	}
	if cfg, _ := store.Save(Patch{BackendURL: &other}); cfg.APIKey != "" {
		t.Errorf("key followed the station to %s: %q", other, cfg.APIKey)
	}
	if cfg, _ := store.Save(Patch{BackendURL: &url, APIKey: &key}); cfg.APIKey != key {
		t.Error("key entered together with the address was dropped")
	}
}

func TestShortAPIKeyIsFullyMasked(t *testing.T) {
	if masked := (Config{APIKey: "abc"}).Public("").APIKeyMasked; masked != "•••" {
		t.Errorf("masked = %q", masked)
	}
}
