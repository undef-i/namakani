package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"namakani/internal/translate"
)

func TestPutTranslateSettingsKillsWorker(t *testing.T) {
	dir := os.Getenv("WMTEST_MODELS_DIR")
	if dir == "" {
		dir = "/tmp/wmtest-data/models"
	}
	if _, err := os.Stat(filepath.Join(dir, "en_zh-Hans", "model.bin")); err != nil {
		t.Skipf("ct2 model pack not available under %s", dir)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if err := exec.Command("python3", "-c", "import ctranslate2, sentencepiece").Run(); err != nil {
		t.Skip("python3 ctranslate2/sentencepiece not available")
	}
	f := newDrainFixture(t)
	f.srv.trMgr = translate.NewManager(dir)
	put := func(engine string) {
		t.Helper()
		req := f.authedReq(http.MethodPut, "/api/settings/translate", `{"engine":"`+engine+`","target":"zh"}`)
		rec := httptest.NewRecorder()
		f.srv.handlePutTranslateSettings(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("put settings engine=%s: status %d, body %s", engine, rec.Code, rec.Body.String())
		}
	}
	eng, err := f.srv.trMgr.EngineFor("en", "zh-Hans")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Translate(context.Background(), []translate.Segment{{ID: 0, Text: "Hello world."}}, "en", "zh-Hans"); err != nil {
		t.Fatal(err)
	}
	pid := f.srv.trMgr.WorkerPID()
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("worker pid after local translate = %d, want a live process", pid)
	}
	put("google")
	if got := f.srv.trMgr.WorkerPID(); got != 0 {
		t.Fatalf("worker pid after switching to google = %d, want 0", got)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("worker pid %d still alive after switching to google", pid)
	}
}
