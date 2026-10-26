package translate

import "sync"

type Manager struct {
	Dir string

	mu     sync.Mutex
	worker *Worker
}

func NewManager(dir string) *Manager {
	return &Manager{Dir: dir}
}

func (m *Manager) getWorker() *Worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worker == nil {
		m.worker = newWorker(m.Dir)
	}
	return m.worker
}

func (m *Manager) CloseWorker() {
	m.mu.Lock()
	w := m.worker
	m.worker = nil
	m.mu.Unlock()
	if w != nil {
		w.Close()
	}
}

func (m *Manager) WorkerPID() int {
	m.mu.Lock()
	w := m.worker
	m.mu.Unlock()
	if w == nil {
		return 0
	}
	return w.PID()
}
