package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"namakani/internal/mail"
	"namakani/internal/proton"
	"namakani/internal/store"
	"namakani/internal/tuta"

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
	Initial   bool
}

type repairInfo struct {
	reason string
	cred   string
}

type Manager struct {
	st          *store.Store
	mu          sync.Mutex
	workers     map[int64]*worker
	status      map[int64]*Status
	live        map[int64]bool
	repair      map[int64]repairInfo
	folderLocks map[int64]*sync.Mutex

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
	retry  chan struct{}
}

func NewManager(st *store.Store) *Manager {
	return &Manager{
		st:      st,
		workers: make(map[int64]*worker),
		status:  make(map[int64]*Status),
		live:    make(map[int64]bool),
		repair:  make(map[int64]repairInfo),
	}
}

func credentialFingerprint(acct mail.Account) string {
	sum := sha256.Sum256([]byte(acct.Username + "\x00" + acct.Password))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) Start(acct mail.Account) {
	m.mu.Lock()
	if w, ok := m.workers[acct.ID]; ok {
		select {
		case w.retry <- struct{}{}:
		default:
		}
		m.mu.Unlock()
		return
	}
	if ri, ok := m.repair[acct.ID]; ok && ri.cred == credentialFingerprint(acct) {
		m.mu.Unlock()
		return
	}
	delete(m.repair, acct.ID)
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{acct: acct, cancel: cancel, retry: make(chan struct{}, 1)}
	m.workers[acct.ID] = w
	m.setStatusLocked(acct.ID, &Status{State: "connecting"})
	m.mu.Unlock()
	go m.run(ctx, w)
}

func (m *Manager) Restart(acct mail.Account) {
	m.mu.Lock()
	delete(m.repair, acct.ID)
	m.mu.Unlock()
	m.Stop(acct.ID)
	m.Start(acct)
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
	out.CacheBudgetBytes = m.st.CacheBudgetBytes()
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

func classifySessionError(acct mail.Account, err error) (bool, string) {
	switch acct.Kind {
	case "proton":
		return proton.ClassifyError(err)
	case "tuta":
		var re *tuta.RestError
		if errors.As(err, &re) && (re.StatusCode == 401 || re.StatusCode == 403) {
			return true, "登录凭据被拒绝或会话已失效，自动重连不可能成功。请在设置里更新密码后点「重新连接」。（" + err.Error() + "）"
		}
	default:
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Code == imap.ResponseCodeAuthenticationFailed {
			return true, "服务器拒绝了登录凭据，自动重连不可能成功。请在设置里更新密码后点「重新连接」。（" + err.Error() + "）"
		}
	}
	return false, ""
}

func (m *Manager) parkRepair(w *worker, reason string) {
	m.mu.Lock()
	if cur, ok := m.workers[w.acct.ID]; ok && cur == w {
		delete(m.workers, w.acct.ID)
	}
	m.repair[w.acct.ID] = repairInfo{reason: reason, cred: credentialFingerprint(w.acct)}
	m.setStatusLocked(w.acct.ID, &Status{State: "repair", LastError: reason})
	m.mu.Unlock()
	w.cancel()
}

func (m *Manager) run(ctx context.Context, w *worker) {
	acct := w.acct
	for {
		m.mu.Lock()
		m.live[acct.ID] = false
		m.mu.Unlock()
		err := m.session(ctx, acct)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		if term, reason := classifySessionError(acct, err); term {
			m.parkRepair(w, reason)
			log.Printf("sync account %d: needs manual repair: %s", acct.ID, reason)
			return
		}
		m.setStatus(acct.ID, &Status{State: "error", LastError: err.Error()})
		log.Printf("sync account %d: %v", acct.ID, err)
		m.mu.Lock()
		lived := m.live[acct.ID]
		m.mu.Unlock()
		if lived {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.retry:
		}
	}
}

type folderRef struct {
	id   int64
	name string
}

func (m *Manager) folderMu(id int64) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.folderLocks == nil {
		m.folderLocks = make(map[int64]*sync.Mutex)
	}
	mu, ok := m.folderLocks[id]
	if !ok {
		mu = &sync.Mutex{}
		m.folderLocks[id] = mu
	}
	return mu
}

func (m *Manager) syncFolderLocked(ctx context.Context, cl mail.Conn, acct mail.Account, r folderRef) (Change, error) {
	mu := m.folderMu(r.id)
	mu.Lock()
	defer mu.Unlock()
	if t, ok := cl.(mail.BackgroundTurner); ok {
		end, ok := t.BackgroundTurn(ctx)
		if !ok {
			return Change{AccountID: acct.ID, FolderID: r.id}, nil
		}
		defer end()
	}
	change, err := m.syncFolder(cl, acct, r.id, r.name)
	if err != nil {
		log.Printf("sync account %d folder %s: %v", acct.ID, r.name, err)
	}
	return change, err
}

func (m *Manager) session(ctx context.Context, acct mail.Account) error {
	if acct.Kind == "proton" {
		return m.protonSession(ctx, acct)
	}
	if acct.Kind == "tuta" {
		return m.tutaSession(ctx, acct)
	}
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
	m.mu.Lock()
	m.live[acct.ID] = false
	m.mu.Unlock()
	m.setStatus(acct.ID, &Status{State: "connecting"})
	cl, err := mail.Connect(acct, handler)
	if err != nil {
		return err
	}
	defer cl.Close()

	listRefs := func() ([]folderRef, *folderRef, error) {
		folders, err := cl.ListFolders()
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

	m.setStatus(acct.ID, &Status{State: "syncing", Folders: len(refs)})
	syncOne := func(r folderRef) { m.syncFolderLocked(ctx, cl, acct, r) }
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
	m.prewarm(ctx, cl, acct)
	m.repairThreads(ctx, cl, acct)

	syncAll := func() {
		for _, r := range refs {
			syncOne(r)
		}
		n, _ := m.st.CountMessages(acct.ID)
		m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix(), Messages: n})
		m.prewarm(ctx, cl, acct)
	}

	if !cl.HasCap(imap.CapIdle) {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
				syncAll()
			case <-time.After(60 * time.Second):
				syncAll()
			}
		}
	}

	wctx, wcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	watched := make(map[int64]context.CancelFunc)
	var watchedMu sync.Mutex
	kick := make(chan struct{}, 1)
	startWatcher := func(r folderRef) {
		watchedMu.Lock()
		defer watchedMu.Unlock()
		if _, ok := watched[r.id]; ok {
			return
		}
		fctx, fcancel := context.WithCancel(wctx)
		watched[r.id] = fcancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.watchFolder(fctx, acct, r, kick)
		}()
	}
	stopWatcher := func(id int64) {
		watchedMu.Lock()
		defer watchedMu.Unlock()
		if c, ok := watched[id]; ok {
			c()
			delete(watched, id)
		}
	}
	for _, r := range refs {
		startWatcher(r)
	}
	defer func() {
		wcancel()
		wg.Wait()
	}()

	sweep := time.NewTicker(10 * time.Minute)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-kick:
			m.prewarm(ctx, cl, acct)
		case <-sweep.C:
			if newRefs, _, err := listRefs(); err == nil {
				seen := make(map[int64]bool, len(newRefs))
				for _, r := range newRefs {
					seen[r.id] = true
				}
				for _, r := range refs {
					if !seen[r.id] {
						stopWatcher(r.id)
					}
				}
				refs = newRefs
				m.setStatus(acct.ID, &Status{State: "idle", Folders: len(refs)})
				for _, r := range refs {
					startWatcher(r)
				}
			} else {
				log.Printf("sync account %d list folders: %v", acct.ID, err)
			}
			syncAll()
		}
	}
}

func (m *Manager) watchFolder(ctx context.Context, acct mail.Account, r folderRef, kick chan<- struct{}) {
	for {
		err := m.watchSession(ctx, acct, r, kick)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("sync account %d folder %s watch: %v", acct.ID, r.name, err)
		}
	}
}

func (m *Manager) watchSession(ctx context.Context, acct mail.Account, r folderRef, kick chan<- struct{}) error {
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
	cl, err := mail.Connect(acct, handler)
	if err != nil {
		return err
	}
	defer cl.Close()

	syncMine := func() {
		change, _ := m.syncFolderLocked(ctx, cl, acct, r)
		m.setStatus(acct.ID, &Status{State: "idle", LastSyncAt: time.Now().Unix()})
		if len(change.Inserted) > 0 {
			select {
			case kick <- struct{}{}:
			default:
			}
		}
	}
	syncMine()

	if !cl.HasCap(imap.CapIdle) {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
				syncMine()
			case <-time.After(60 * time.Second):
				syncMine()
			}
		}
	}

	for {
		idleCmd, err := cl.Idle()
		if err != nil {
			return fmt.Errorf("idle: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- idleCmd.Wait() }()
		poll := time.NewTimer(2 * time.Minute)
		var idleErr error
		gotDone := false
	idleWait:
		for {
			select {
			case <-ctx.Done():
				idleCmd.Close()
				<-done
				poll.Stop()
				return nil
			case <-wake:
				break idleWait
			case <-poll.C:
				break idleWait
			case err := <-done:
				idleErr = err
				gotDone = true
				break idleWait
			}
		}
		poll.Stop()
		idleCmd.Close()
		if !gotDone {
			<-done
		}
		if idleErr != nil {
			return fmt.Errorf("idle wait: %w", idleErr)
		}
		syncMine()
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

func (m *Manager) syncFolder(cl mail.Conn, acct mail.Account, folderID int64, name string) (Change, error) {
	m.mu.Lock()
	live := m.live[acct.ID]
	m.mu.Unlock()
	change := Change{AccountID: acct.ID, FolderID: folderID, Initial: !live}
	defer func() { m.emit(change) }()
	state, err := m.st.FolderState(folderID)
	if err != nil {
		return change, err
	}
	sel, err := cl.Select(name)
	if err != nil {
		return change, err
	}
	uidNext := uint32(sel.UIDNext)
	if state.UIDValidity != 0 && state.UIDValidity != sel.UIDValidity {
		if err := m.st.WipeFolder(folderID); err != nil {
			return change, err
		}
		state, err = m.st.FolderState(folderID)
		if err != nil {
			return change, err
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
			uids := make([]uint32, 0, end-start+1)
			for u := start; u <= end; u++ {
				uids = append(uids, u)
			}
			msgs, err := cl.FetchEnvelopes(uids)
			if err != nil {
				return change, fmt.Errorf("fetch envelopes %d:%d: %w", start, end, err)
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
					return change, fmt.Errorf("upsert message: %w", err)
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
			return change, fmt.Errorf("fetch changed flags: %w", err)
		}
		for _, msg := range changed {
			didChange, err := m.st.UpdateMessageFlags(folderID, msg.UID, msg.Flags, msg.ModSeq)
			if err != nil {
				return change, err
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
		lister, ok := cl.(mail.UIDLister)
		if !ok {
			return change, fmt.Errorf("connection claims CONDSTORE but cannot list uids")
		}
		uids, err := lister.FetchUIDs()
		if err != nil {
			return change, fmt.Errorf("fetch uids: %w", err)
		}
		seen := make(map[uint32]bool, len(uids))
		for _, uid := range uids {
			seen[uid] = true
		}
		if err := m.reconcileDeletions(folderID, seen, &change); err != nil {
			return change, err
		}
	} else {
		all, err := cl.FetchFlags(0)
		if err != nil {
			return change, fmt.Errorf("fetch flags: %w", err)
		}
		seen := make(map[uint32]bool, len(all))
		for _, msg := range all {
			seen[msg.UID] = true
			didChange, err := m.st.UpdateMessageFlags(folderID, msg.UID, msg.Flags, msg.ModSeq)
			if err != nil {
				return change, err
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
		if err := m.reconcileDeletions(folderID, seen, &change); err != nil {
			return change, err
		}
	}

	err = m.st.UpdateFolderSync(folderID, sel.UIDValidity, uidNext, highestModSeq, int(sel.NumMessages))
	return change, err
}

func (m *Manager) reconcileDeletions(folderID int64, seen map[uint32]bool, change *Change) error {
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
	return nil
}
