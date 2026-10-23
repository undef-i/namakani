package sync

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/emersion/hydroxide/protonmail"

	"namakani/internal/mail"
	"namakani/internal/proton"
)

func (m *Manager) protonSession(ctx context.Context, acct mail.Account) error {
	sess, err := proton.Connect(m.st, acct)
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

	syncNames := func(names map[string]bool) {
		for _, r := range refs {
			if names[r.name] {
				syncOne(r)
			}
		}
		n, _ := m.st.CountMessages(acct.ID)
		m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix(), Messages: n})
		m.prewarm(ctx, sess, acct)
	}
	syncAll := func() {
		for _, r := range refs {
			syncOne(r)
		}
		n, _ := m.st.CountMessages(acct.ID)
		m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix(), Messages: n})
		m.prewarm(ctx, sess, acct)
	}

	events := make(chan *protonmail.Event, 4)
	go func() {
		last, err := sess.EventLatest()
		if err != nil {
			log.Printf("sync account %d proton events baseline: %v", acct.ID, err)
			last = ""
		}
		for {
			if ctx.Err() != nil {
				return
			}
			ev, err := sess.EventSince(last)
			if err != nil {
				log.Printf("sync account %d proton events: %v", acct.ID, err)
				if base, berr := sess.EventLatest(); berr == nil {
					last = base
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(15 * time.Second):
				}
				continue
			}
			if ev == nil {
				continue
			}
			changed := ev.ID != last || len(ev.Messages) > 0 || ev.Refresh != 0
			if ev.ID != "" {
				last = ev.ID
			}
			if !changed {
				select {
				case <-ctx.Done():
					return
				case <-time.After(15 * time.Second):
				}
				continue
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	sweep := time.NewTicker(10 * time.Minute)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sweep.C:
			sess.InvalidateFolders()
			if newRefs, _, err := listRefs(); err == nil {
				refs = newRefs
				m.setStatus(acct.ID, &Status{State: "idle", Folders: len(refs)})
			} else {
				log.Printf("sync account %d list folders: %v", acct.ID, err)
			}
			syncAll()
		case ev := <-events:
			if ev.Refresh&protonmail.EventRefreshMail != 0 {
				syncAll()
				continue
			}
			affected, err := m.protonAffectedFolders(acct, sess, ev)
			if err != nil {
				log.Printf("sync account %d proton events map: %v", acct.ID, err)
				syncAll()
				continue
			}
			if len(affected) == 0 {
				continue
			}
			syncNames(affected)
		}
	}
}

func (m *Manager) protonAffectedFolders(acct mail.Account, sess *proton.Session, ev *protonmail.Event) (map[string]bool, error) {
	pairs, err := sess.FolderLabelPairs()
	if err != nil {
		return nil, err
	}
	byLabel := make(map[string]string, len(pairs))
	for name, label := range pairs {
		byLabel[label] = name
	}
	affected := make(map[string]bool)
	mark := func(labelID string) {
		if name, ok := byLabel[labelID]; ok {
			affected[name] = true
		}
	}
	for _, em := range ev.Messages {
		switch em.Action {
		case protonmail.EventCreate:
			if em.Created != nil {
				for _, lid := range em.Created.LabelIDs {
					mark(lid)
				}
			}
		case protonmail.EventUpdate, protonmail.EventUpdateFlags:
			if em.Updated != nil {
				added, removed := em.Updated.DiffLabelIDs(nil)
				for _, lid := range added {
					mark(lid)
				}
				for _, lid := range removed {
					mark(lid)
				}
				for _, lid := range em.Updated.LabelIDs {
					mark(lid)
				}
			}
		}
	}
	counts, err := sess.MessageCounts()
	if err != nil {
		return nil, err
	}
	totals, err := m.st.FolderTotals(acct.ID)
	if err != nil {
		return nil, err
	}
	for name, label := range pairs {
		if c, ok := counts[label]; ok {
			if t, ok := totals[name]; ok && t != c {
				affected[name] = true
			}
		}
	}
	return affected, nil
}
