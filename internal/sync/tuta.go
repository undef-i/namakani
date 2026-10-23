package sync

import (
	"context"
	"fmt"
	"time"

	"namakani/internal/mail"
	"namakani/internal/tuta"
)

func (m *Manager) tutaSession(ctx context.Context, acct mail.Account, releaseInit func()) error {
	sess, err := tuta.Connect(m.st, acct)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.live[acct.ID] = false
	m.mu.Unlock()
	m.setStatus(acct.ID, &Status{State: "syncing"})

	listRefs := func() ([]folderRef, *folderRef, error) {
		folders, err := sess.ListFolders()
		if err != nil {
			return nil, nil, err
		}
		refs := make([]folderRef, 0, len(folders))
		var inbox *folderRef
		for _, f := range folders {
			id, err := m.st.UpsertFolder(acct.ID, f.Name, f.SpecialUse)
			if err != nil {
				return nil, nil, err
			}
			r := folderRef{id: id, name: f.Name}
			refs = append(refs, r)
			if f.SpecialUse == "inbox" && inbox == nil {
				rr := r
				inbox = &rr
			}
		}
		return refs, inbox, nil
	}
	refs, inboxRef, err := listRefs()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	syncOne := func(r folderRef) error {
		_, err := m.syncFolderLocked(ctx, sess, acct, r)
		return err
	}
	var syncErr error
	noteErr := func(err error) {
		if err != nil && syncErr == nil {
			syncErr = err
		}
	}
	if inboxRef != nil {
		noteErr(syncOne(*inboxRef))
	}
	for _, r := range refs {
		if inboxRef != nil && r.id == inboxRef.id {
			continue
		}
		noteErr(syncOne(r))
	}
	n, _ := m.st.CountMessages(acct.ID)
	if syncErr != nil {
		m.setStatus(acct.ID, &Status{
			State:      "error",
			LastSyncAt: time.Now().Unix(),
			LastError:  syncErr.Error(),
			Folders:    len(refs),
			Messages:   n,
		})
	} else {
		m.setStatus(acct.ID, &Status{
			State:      "idle",
			LastSyncAt: time.Now().Unix(),
			Folders:    len(refs),
			Messages:   n,
		})
	}
	m.mu.Lock()
	m.live[acct.ID] = true
	m.mu.Unlock()
	m.prewarm(ctx, sess, acct)
	m.repairThreads(ctx, sess, acct)
	releaseInit()

	syncAll := func() {
		if newRefs, _, err := listRefs(); err == nil {
			refs = newRefs
		}
		var firstErr error
		for _, r := range refs {
			if err := syncOne(r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		n, _ := m.st.CountMessages(acct.ID)
		if firstErr != nil {
			m.setStatus(acct.ID, &Status{State: "error", LastSyncAt: time.Now().Unix(), LastError: firstErr.Error(), Folders: len(refs), Messages: n})
		} else {
			m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix(), Folders: len(refs), Messages: n})
		}
		m.prewarm(ctx, sess, acct)
	}

	events := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go sess.WatchEvents(stop, func() {
		select {
		case events <- struct{}{}:
		default:
		}
	})

	sweep := time.NewTicker(10 * time.Minute)
	defer sweep.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sweep.C:
			syncAll()
		case <-events:
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.NewTimer(2 * time.Second)
			debounceC = debounce.C
		case <-debounceC:
			debounce = nil
			debounceC = nil
			syncAll()
		}
	}
}
