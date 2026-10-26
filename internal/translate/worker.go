package translate

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

//go:embed worker/ct2_worker.py
var workerScript []byte

type workerRequest struct {
	ID       int      `json:"id"`
	Dir      string   `json:"dir"`
	Segments []string `json:"segments"`
}

type workerResponse struct {
	ID       int      `json:"id"`
	Segments []string `json:"segments"`
	Error    string   `json:"error"`
}

type Worker struct {
	modelsDir string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	stdout *bufio.Reader
	seq    int
}

func newWorker(modelsDir string) *Worker {
	return &Worker{modelsDir: modelsDir}
}

func (w *Worker) scriptPath() string {
	return filepath.Join(filepath.Dir(w.modelsDir), "ct2_worker.py")
}

func (w *Worker) ensureScript() error {
	p := w.scriptPath()
	if raw, err := os.ReadFile(p); err == nil && bytes.Equal(raw, workerScript) {
		return nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, workerScript, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (w *Worker) startLocked() error {
	if err := w.ensureScript(); err != nil {
		return fmt.Errorf("ct2 worker script: %w", err)
	}
	cmd := exec.Command("python3", w.scriptPath())
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ct2 worker: %w", err)
	}
	w.cmd = cmd
	w.stdin = bufio.NewWriter(stdin)
	w.stdout = bufio.NewReader(stdout)
	return nil
}

func (w *Worker) stopLocked() {
	if w.cmd == nil {
		return
	}
	if w.cmd.Process != nil {
		w.cmd.Process.Kill()
	}
	w.cmd.Wait()
	w.cmd = nil
	w.stdin = nil
	w.stdout = nil
}

func (w *Worker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

func (w *Worker) PID() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil || w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

func (w *Worker) Translate(ctx context.Context, pairDir string, texts []string) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil {
		if err := w.startLocked(); err != nil {
			return nil, err
		}
	}
	w.seq++
	id := w.seq
	raw, err := json.Marshal(workerRequest{ID: id, Dir: pairDir, Segments: texts})
	if err != nil {
		return nil, err
	}
	if _, err := w.stdin.Write(append(raw, '\n')); err != nil {
		w.stopLocked()
		return nil, fmt.Errorf("ct2 worker write: %w", err)
	}
	if err := w.stdin.Flush(); err != nil {
		w.stopLocked()
		return nil, fmt.Errorf("ct2 worker write: %w", err)
	}
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := w.stdout.ReadBytes('\n')
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		w.stopLocked()
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			w.stopLocked()
			return nil, fmt.Errorf("ct2 worker read: %w", res.err)
		}
		var resp workerResponse
		if err := json.Unmarshal(res.line, &resp); err != nil {
			w.stopLocked()
			return nil, fmt.Errorf("ct2 worker response: %w", err)
		}
		if resp.ID != id {
			w.stopLocked()
			return nil, fmt.Errorf("ct2 worker response id %d, want %d", resp.ID, id)
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("ct2 worker: %s", resp.Error)
		}
		return resp.Segments, nil
	}
}
