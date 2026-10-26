package translate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode"
)

func ct2ModelsDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("WMTEST_MODELS_DIR")
	if dir == "" {
		dir = "/tmp/wmtest-data/models"
	}
	if _, err := os.Stat(filepath.Join(dir, "en_zh-Hans", "model.bin")); err != nil {
		t.Skipf("ct2 model pack not available under %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "zh-Hans_en", "model.bin")); err != nil {
		t.Skipf("ct2 zh->en pack not available under %s", dir)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if err := exec.Command("python3", "-c", "import ctranslate2, sentencepiece").Run(); err != nil {
		t.Skip("python3 ctranslate2/sentencepiece not available")
	}
	return dir
}

func pidAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func TestWorkerTranslatesBothDirections(t *testing.T) {
	m := NewManager(ct2ModelsDir(t))
	ctx := context.Background()

	eng, err := m.EngineFor("en", "zh-Hans")
	if err != nil {
		t.Fatal(err)
	}
	out, err := eng.Translate(ctx, []Segment{{ID: 7, Text: "Hello, thank you for your email."}}, "en", "zh-Hans")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != 7 {
		t.Fatalf("en->zh output = %+v, want one segment with ID 7", out)
	}
	if out[0].Text == "" || out[0].Text == "Hello, thank you for your email." {
		t.Fatalf("en->zh output not translated: %q", out[0].Text)
	}
	hasHan := false
	for _, r := range out[0].Text {
		if unicode.Is(unicode.Han, r) {
			hasHan = true
			break
		}
	}
	if !hasHan {
		t.Fatalf("en->zh output has no Han characters: %q", out[0].Text)
	}

	eng2, err := m.EngineFor("zh-Hans", "en")
	if err != nil {
		t.Fatal(err)
	}
	out2, err := eng2.Translate(ctx, []Segment{{ID: 3, Text: "你好，谢谢你的邮件。"}}, "zh-Hans", "en")
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) != 1 || out2[0].ID != 3 || !strings.ContainsAny(out2[0].Text, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatalf("zh->en output = %+v, want one English segment with ID 3", out2)
	}

	pid1 := m.WorkerPID()
	if !pidAlive(pid1) {
		t.Fatalf("worker pid %d not alive after translations", pid1)
	}
	m.CloseWorker()
	if got := m.WorkerPID(); got != 0 {
		t.Fatalf("worker pid after CloseWorker = %d, want 0", got)
	}
	if pidAlive(pid1) {
		t.Fatalf("worker pid %d still alive after CloseWorker", pid1)
	}
	out3, err := eng.Translate(ctx, []Segment{{ID: 9, Text: "Good morning."}}, "en", "zh-Hans")
	if err != nil {
		t.Fatalf("translate after CloseWorker: %v", err)
	}
	if len(out3) != 1 || out3[0].Text == "" {
		t.Fatalf("translate after CloseWorker output = %+v", out3)
	}
	pid2 := m.WorkerPID()
	if !pidAlive(pid2) || pid2 == pid1 {
		t.Fatalf("respawned worker pid = %d, want a new live pid (old %d)", pid2, pid1)
	}
	m.CloseWorker()
}
