package store

import (
	"path/filepath"
	"testing"
)

func reopenStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func TestBaseURLMigratesFromTGPublicBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namakani.db")
	st := reopenStore(t, path)
	if err := st.SetSetting("tg_public_base", "https://mail.example.com"); err != nil {
		t.Fatalf("seed tg_public_base: %v", err)
	}
	st.DB.Close()

	st = reopenStore(t, path)
	defer st.DB.Close()
	if _, ok, err := st.GetSetting("tg_public_base"); err != nil || ok {
		t.Fatalf("tg_public_base after migration: ok=%v err=%v; want the key gone", ok, err)
	}
	v, ok, err := st.GetSetting("base_url")
	if err != nil || !ok || v != "https://mail.example.com" {
		t.Fatalf("base_url = %q, ok=%v, err=%v; want the migrated tg_public_base value", v, ok, err)
	}
	if got := st.BaseURL(); got != "https://mail.example.com" {
		t.Fatalf("BaseURL() = %q; want https://mail.example.com", got)
	}
}

func TestBaseURLMigrationKeepsExistingBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namakani.db")
	st := reopenStore(t, path)
	if err := st.SetSetting("base_url", "https://new.example.com"); err != nil {
		t.Fatalf("seed base_url: %v", err)
	}
	if err := st.SetSetting("tg_public_base", "https://old.example.com"); err != nil {
		t.Fatalf("seed tg_public_base: %v", err)
	}
	st.DB.Close()

	st = reopenStore(t, path)
	defer st.DB.Close()
	if _, ok, err := st.GetSetting("tg_public_base"); err != nil || ok {
		t.Fatalf("tg_public_base after migration: ok=%v err=%v; want the key gone", ok, err)
	}
	if got := st.BaseURL(); got != "https://new.example.com" {
		t.Fatalf("BaseURL() = %q; want the pre-existing base_url to win", got)
	}
}

func TestBaseURLAppSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namakani.db")
	st := reopenStore(t, path)
	cfg := st.AppSettings()
	cfg.BaseURL = "https://mail.example.com/"
	if err := st.SaveAppSettings(cfg); err != nil {
		t.Fatalf("save app settings: %v", err)
	}
	if got := st.BaseURL(); got != "https://mail.example.com" {
		t.Fatalf("BaseURL() = %q; want the saved value without a trailing slash", got)
	}
	st.DB.Close()

	st = reopenStore(t, path)
	defer st.DB.Close()
	if got := st.BaseURL(); got != "https://mail.example.com" {
		t.Fatalf("BaseURL() after reopen = %q; want the persisted value", got)
	}
}
