package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type Status struct {
	State      string `json:"state"`
	LastSyncAt int64  `json:"last_sync_at"`
	LastError  string `json:"last_error"`
	Folders    int    `json:"folders"`
	Messages   int    `json:"messages"`

	CacheBodiesCached      int   `json:"cache_bodies_cached"`
	CacheBodiesTotal       int   `json:"cache_bodies_total"`
	CacheAttachmentsCached int   `json:"cache_attachments_cached"`
	CacheAttachmentsTotal  int   `json:"cache_attachments_total"`
	CacheBytesUsed         int64 `json:"cache_bytes_used"`
	CacheBudgetBytes       int64 `json:"cache_budget_bytes"`
}

type Change struct {
	AccountID int64
	FolderID  int64
	Inserted  []int64
	Flagged   []int64
	Removed   []int64
}

type Manager struct {
	st      *store.Store
	mu      sync.Mutex
	workers map[int64]*worker
	status  map[int64]*Status

	OnChange func(Change)
}

func (m *Manager) emit(c Change) {
	if m.OnChange != nil && (len(c.Inserted) > 0 || len(c.Flagged) > 0 || len(c.Removed) > 0) {
		m.OnChange(c)
	}
}

type worker struct {
	acct   mail.Account
	cancel context.CancelFunc
}

func NewManager(st *store.Store) *Manager {
	return &Manager{
		st:      st,
		workers: make(map[int64]*worker),
		status:  make(map[int64]*Status),
	}
}

func (m *Manager) Start(acct mail.Account) {
	m.mu.Lock()
	if _, ok := m.workers[acct.ID]; ok {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.workers[acct.ID] = &worker{acct: acct, cancel: cancel}
	m.setStatusLocked(acct.ID, &Status{State: "connecting"})
	m.mu.Unlock()
	go m.run(ctx, acct)
}

func (m *Manager) Stop(accountID int64) {
	m.mu.Lock()
	if w, ok := m.workers[accountID]; ok {
		w.cancel()
		delete(m.workers, accountID)
	}
	m.setStatusLocked(accountID, &Status{State: "stopped"})
	m.mu.Unlock()
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]int64, 0, len(m.workers))
	for id, w := range m.workers {
		w.cancel()
		ids = append(ids, id)
	}
	m.workers = make(map[int64]*worker)
	for _, id := range ids {
		m.setStatusLocked(id, &Status{State: "stopped"})
	}
	m.mu.Unlock()
}

func (m *Manager) Status(accountID int64) Status {
	m.mu.Lock()
	var out Status
	if s, ok := m.status[accountID]; ok {
		out = *s
	} else {
		out = Status{State: "stopped"}
	}
	m.mu.Unlock()
	if cs, err := m.st.AccountCacheStats(accountID); err == nil {
		out.CacheBodiesCached = cs.BodiesCached
		out.CacheBodiesTotal = cs.BodiesTotal
		out.CacheAttachmentsCached = cs.AttachmentsCached
		out.CacheAttachmentsTotal = cs.AttachmentsTotal
	}
	if bodyBytes, attBytes, err := m.st.CacheUsage(); err == nil {
		out.CacheBytesUsed = bodyBytes + attBytes
	}
	out.CacheBudgetBytes = store.CacheBudgetBytes
	return out
}

func (m *Manager) setStatusLocked(id int64, s *Status) {
	if old, ok := m.status[id]; ok {
		if s.LastSyncAt == 0 {
			s.LastSyncAt = old.LastSyncAt
		}
		if s.Folders == 0 {
			s.Folders = old.Folders
		}
		if s.Messages == 0 {
			s.Messages = old.Messages
		}
	}
	m.status[id] = s
}

func (m *Manager) setStatus(id int64, s *Status) {
	m.mu.Lock()
	m.setStatusLocked(id, s)
	m.mu.Unlock()
}

func (m *Manager) run(ctx context.Context, acct mail.Account) {
	backoff := 5 * time.Second
	for {
		err := m.session(ctx, acct)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.setStatus(acct.ID, &Status{State: "error", LastError: err.Error()})
			log.Printf("sync account %d: %v (retry in %s)", acct.ID, err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err != nil {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		} else {
			backoff = 5 * time.Second
		}
	}
}

func (m *Manager) session(ctx context.Context, acct mail.Account) error {
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	handler := &imapclient.UnilateralDataHandler{
		Mailbox: func(*imapclient.UnilateralDataMailbox) { notify() },
		Fetch:   func(*imapclient.FetchMessageData) { notify() },
		Expunge: func(uint32) { notify() },
	}
	m.setStatus(acct.ID, &Status{State: "connecting"})
	cl, err := mail.Connect(acct, handler)
	if err != nil {
		return err
	}
	defer cl.Close()

	folders, err := cl.ListFolders()
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}
	type folderRef struct {
		id   int64
		name string
	}
	refs := make([]folderRef, 0, len(folders))
	var inboxRef *folderRef
	for _, f := range folders {
		id, err := m.st.UpsertFolder(acct.ID, f.Name, f.SpecialUse)
		if err != nil {
			return fmt.Errorf("upsert folder: %w", err)
		}
		refs = append(refs, folderRef{id: id, name: f.Name})
		if f.SpecialUse == "inbox" && inboxRef == nil {
			r := folderRef{id: id, name: f.Name}
			inboxRef = &r
		}
	}

	m.setStatus(acct.ID, &Status{State: "syncing", Folders: len(refs)})
	syncOne := func(r folderRef) {
		if err := m.syncFolder(cl, acct, r.id, r.name); err != nil {
			log.Printf("sync account %d folder %s: %v", acct.ID, r.name, err)
		}
	}
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
	m.prewarm(ctx, cl, acct)

	syncAll := func() {
		for _, r := range refs {
			syncOne(r)
		}
		n, _ := m.st.CountMessages(acct.ID)
		m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix(), Messages: n})
		m.prewarm(ctx, cl, acct)
	}

	hasIdle := cl.HasCap(imap.CapIdle)
	for {
		if inboxRef == nil || !hasIdle {
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
				syncAll()
			case <-time.After(60 * time.Second):
				syncAll()
			}
			continue
		}
		if _, err := cl.Select(inboxRef.name); err != nil {
			return fmt.Errorf("select inbox: %w", err)
		}
		idleCmd, err := cl.Idle()
		if err != nil {
			return fmt.Errorf("idle: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- idleCmd.Wait() }()
		poll := time.NewTimer(2 * time.Minute)
		sweep := time.NewTimer(10 * time.Minute)
		stop := false
		for !stop {
			select {
			case <-ctx.Done():
				idleCmd.Close()
				<-done
				return nil
			case <-wake:
				stop = true
			case <-poll.C:
				stop = true
			case <-sweep.C:
				idleCmd.Close()
				<-done
				syncAll()
				stop = true
				continue
			case err := <-done:
				if err != nil {
					return fmt.Errorf("idle wait: %w", err)
				}
				stop = true
				continue
			}
			idleCmd.Close()
			<-done
		}
		poll.Stop()
		sweep.Stop()
		if err := m.syncFolder(cl, acct, inboxRef.id, inboxRef.name); err != nil {
			log.Printf("sync account %d inbox: %v", acct.ID, err)
		}
		m.prewarm(ctx, cl, acct)
	}
}

func addrsJSON(addrs []mail.Addr) string {
	if len(addrs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(addrs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func (m *Manager) syncFolder(cl *mail.Client, acct mail.Account, folderID int64, name string) error {
	change := Change{AccountID: acct.ID, FolderID: folderID}
	defer func() { m.emit(change) }()
	state, err := m.st.FolderState(folderID)
	if err != nil {
		return err
	}
	sel, err := cl.Select(name)
	if err != nil {
		return err
	}
	uidNext := uint32(sel.UIDNext)
	if state.UIDValidity != 0 && state.UIDValidity != sel.UIDValidity {
		if err := m.st.WipeFolder(folderID); err != nil {
			return err
		}
		state, err = m.st.FolderState(folderID)
		if err != nil {
			return err
		}
	}

	if uidNext > state.UIDNext {
		from := state.UIDNext
		if from == 0 {
			from = 1
		}
		for start := from; start < uidNext; {
			end := start + 199
			if end >= uidNext {
				end = uidNext - 1
			}
			set := imap.UIDSet{}
			set.AddRange(imap.UID(start), imap.UID(end))
			msgs, err := cl.FetchEnvelopes(set)
			if err != nil {
				return fmt.Errorf("fetch envelopes %d:%d: %w", start, end, err)
			}
			for _, msg := range msgs {
				row := &store.MessageRow{
					UID:       msg.UID,
					MessageID: msg.MessageID,
					ThreadKey: m.st.ComputeThreadKey(acct.ID, msg.MessageID, msg.InReplyTo),
					Subject:   msg.Subject,
					FromJSON:  addrsJSON(msg.From),
					ToJSON:    addrsJSON(msg.To),
					CcJSON:    addrsJSON(msg.Cc),
					DateTS:    msg.Date.Unix(),
					Flags:     msg.Flags,
					HasAttach: msg.HasAttach,
					Size:      msg.Size,
					ModSeq:    msg.ModSeq,
				}
				id, inserted, err := m.st.UpsertMessage(acct.ID, folderID, row)
				if err != nil {
					return fmt.Errorf("upsert message: %w", err)
				}
				if inserted {
					change.Inserted = append(change.Inserted, id)
					if row.ThreadKey != "" && msg.MessageID != "" {
						_ = m.st.AdoptThreadKey(acct.ID, msg.MessageID, row.ThreadKey, id)
					}
				}
				if msg.ModSeq > state.HighestModSeq {
					state.HighestModSeq = msg.ModSeq
				}
			}
			start = end + 1
		}
	}

	var highestModSeq = state.HighestModSeq
	if cl.HasCap(imap.CapCondStore) && state.HighestModSeq > 0 && state.UIDNext > 0 {
		changed, err := cl.FetchFlags(state.HighestModSeq)
		if err != nil {
			return fmt.Errorf("fetch changed flags: %w", err)
		}
		for _, msg := range changed {
			didChange, err := m.st.UpdateMessageFlags(folderID, msg.UID, msg.Flags, msg.ModSeq)
			if err != nil {
				return err
			}
			if didChange {
				if id, ok := m.st.LocalID(folderID, msg.UID); ok {
					change.Flagged = append(change.Flagged, id)
				}
			}
			if msg.ModSeq > highestModSeq {
				highestModSeq = msg.ModSeq
			}
		}
	} else {
		all, err := cl.FetchFlags(0)
		if err != nil {
			return fmt.Errorf("fetch flags: %w", err)
		}
		seen := make(map[uint32]bool, len(all))
		for _, msg := range all {
			seen[msg.UID] = true
			didChange, err := m.st.UpdateMessageFlags(folderID, msg.UID, msg.Flags, msg.ModSeq)
			if err != nil {
				return err
			}
			if didChange {
				if id, ok := m.st.LocalID(folderID, msg.UID); ok {
					change.Flagged = append(change.Flagged, id)
				}
			}
			if msg.ModSeq > highestModSeq {
				highestModSeq = msg.ModSeq
			}
		}
		local, err := m.st.FolderUIDs(folderID)
		if err != nil {
			return err
		}
		for uid := range local {
			if !seen[uid] {
				if id, ok := m.st.LocalID(folderID, uid); ok {
					change.Removed = append(change.Removed, id)
				}
				if err := m.st.DeleteMessage(folderID, uid); err != nil {
					return err
				}
			}
		}
	}

	return m.st.UpdateFolderSync(folderID, sel.UIDValidity, uidNext, highestModSeq, int(sel.NumMessages))
}
