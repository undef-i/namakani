package sync

import (
	"context"
	"fmt"
	"time"

	"namakani/internal/mail"
	"namakani/internal/tuta"
)

func (m *Manager) tutaSession(ctx context.Context, acct mail.Account) error {
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

	syncOne := func(r folderRef) { m.syncFolderLocked(ctx, sess, acct, r) }
	if inboxRef != nil {
		syncOne(*inboxRef)
	}
	for _, r := range refs {
		if inboxRef != nil && r.id == inboxRef.id {
			continue
		}
		syncOne(r)
	}
	n, _ := m.st.CountMessages(acct.ID)
	m.setStatus(acct.ID, &Status{
		State:      "idle",
		LastSyncAt: time.Now().Unix(),
		Folders:    len(refs),
		Messages:   n,
	})
	m.mu.Lock()
	m.live[acct.ID] = true
	m.mu.Unlock()
	m.prewarm(ctx, sess, acct)
	m.repairThreads(ctx, sess, acct)

	syncAll := func() {
		if newRefs, _, err := listRefs(); err == nil {
			refs = newRefs
		}
		for _, r := range refs {
			syncOne(r)
		}
		n, _ := m.st.CountMessages(acct.ID)
		m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix(), Folders: len(refs), Messages: n})
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
