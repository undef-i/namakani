package translate

import (
	"os"
	"path/filepath"
	"testing"
)

func writePack(t *testing.T, modelsDir, name string, omit string) {
	t.Helper()
	dir := filepath.Join(modelsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]int{
		"model.bin":              100,
		"shared_vocabulary.json": 10,
		"source.spm":             20,
		"target.spm":             30,
	}
	for fn, size := range files {
		if fn == omit {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, fn), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiskPairDiscovery(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "en_zh-Hans", "")
	writePack(t, dir, "zh-Hans_en", "")
	writePack(t, dir, "de_en", "target.spm")
	bad := filepath.Join(dir, "nounderscore")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fr_en"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	pairs, err := m.Pairs()
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("discovered %d pairs, want 2: %+v", len(pairs), pairs)
	}
	if pairs[0].ID != "en_zh-Hans" || pairs[1].ID != "zh-Hans_en" {
		t.Fatalf("pair order = %s, %s; want en_zh-Hans, zh-Hans_en", pairs[0].ID, pairs[1].ID)
	}
	if pairs[0].From != "en" || pairs[0].To != "zh-Hans" {
		t.Fatalf("en_zh-Hans parsed as %s -> %s", pairs[0].From, pairs[0].To)
	}
	if pairs[1].From != "zh-Hans" || pairs[1].To != "en" {
		t.Fatalf("zh-Hans_en parsed as %s -> %s", pairs[1].From, pairs[1].To)
	}
	if pairs[0].DiskBytes != 160 {
		t.Fatalf("disk bytes = %d, want 160", pairs[0].DiskBytes)
	}
}

func TestEngineForDiskPairs(t *testing.T) {
	dir := t.TempDir()
	writePack(t, dir, "en_zh-Hans", "")
	writePack(t, dir, "zh-Hans_en", "")
	writePack(t, dir, "en_de", "")
	m := NewManager(dir)
	if _, err := m.EngineFor("en", "zh"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EngineFor("zh", "de"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EngineFor("fr", "de"); err == nil {
		t.Fatal("EngineFor(fr, de) succeeded with no fr packs on disk")
	}
	if _, err := m.EngineFor("en", "fr"); err == nil {
		t.Fatal("EngineFor(en, fr) succeeded with no fr pack on disk")
	}
}
