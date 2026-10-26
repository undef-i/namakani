package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
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

type CrossRemoval struct {
	FolderID int64
	IDs      []int64
}

type Change struct {
	AccountID    int64
	FolderID     int64
	Inserted     []int64
	Flagged      []int64
	Removed      []int64
	CrossRemoved []CrossRemoval
	Initial      bool
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
	lastCache   map[int64]store.AccountCacheStats
	lastBytes   int64
	live        map[int64]bool
	repair      map[int64]repairInfo
	folderLocks map[int64]*sync.Mutex
	initTurn    chan struct{}
	ftsOnce     sync.Once

	OnChange func(Change)
}

func (m *Manager) emit(c Change) {
	if m.OnChange != nil && (len(c.Inserted) > 0 || len(c.Flagged) > 0 || len(c.Removed) > 0 || len(c.CrossRemoved) > 0) {
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
		st:       st,
		workers:  make(map[int64]*worker),
		status:   make(map[int64]*Status),
		lastCache: make(map[int64]store.AccountCacheStats),
		live:     make(map[int64]bool),
		repair:   make(map[int64]repairInfo),
		initTurn: make(chan struct{}, 1),
	}
}

func (m *Manager) acquireInitTurn(ctx context.Context) (func(), bool) {
	select {
	case m.initTurn <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-m.initTurn }) }, true
	case <-ctx.Done():
		return nil, false
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
	m.ftsOnce.Do(func() { go m.backfillFTSHTML() })
	go m.run(ctx, w)
}

func (m *Manager) backfillFTSHTML() {
	if v, ok, err := m.st.GetSetting("fts_html_text_v1"); err == nil && ok && v == "1" {
		return
	}
	n, err := m.st.BackfillFTSHTMLBodies()
	if err != nil {
		log.Printf("fts html backfill: %v", err)
		return
	}
	if err := m.st.SetSetting("fts_html_text_v1", "1"); err != nil {
		log.Printf("fts html backfill marker: %v", err)
		return
	}
	if n > 0 {
		log.Printf("fts html backfill: indexed text for %d html-only bodies", n)
	}
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
	cs, csErr := m.st.AccountCacheStats(accountID)
	bodyBytes, attBytes, usageErr := m.st.CacheUsage()
	m.mu.Lock()
	if csErr == nil {
		m.lastCache[accountID] = cs
	} else if last, ok := m.lastCache[accountID]; ok {
		cs = last
		csErr = nil
	}
	if usageErr == nil {
		m.lastBytes = bodyBytes + attBytes
	}
	out.CacheBytesUsed = m.lastBytes
	m.mu.Unlock()
	if csErr == nil {
		out.CacheBodiesCached = cs.BodiesCached
		out.CacheBodiesTotal = cs.BodiesTotal
		out.CacheAttachmentsCached = cs.AttachmentsCached
		out.CacheAttachmentsTotal = cs.AttachmentsTotal
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
			return true, "保存的登录凭据已被拒绝，请更新密码后点「重新连接」。"
		}
	default:
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Code == imap.ResponseCodeAuthenticationFailed {
			return true, "保存的登录凭据已被拒绝，请更新密码后点「重新连接」。"
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
	failures := 0
	for {
		m.mu.Lock()
		m.live[acct.ID] = false
		m.mu.Unlock()
		err := m.session(ctx, acct)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
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
			failures = 0
			continue
		}
		failures++
		timer := time.NewTimer(time.Duration(store.BackoffSeconds(failures)) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.retry:
			timer.Stop()
		case <-timer.C:
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
	release, ok := m.acquireInitTurn(ctx)
	if !ok {
		return ctx.Err()
	}
	defer release()
	if acct.Kind == "proton" {
		return m.protonSession(ctx, acct, release)
	}
	if acct.Kind == "tuta" {
		return m.tutaSession(ctx, acct, release)
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
	release()
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

const (
	watchRetryBase = 5 * time.Second
	watchRetryCap  = 2 * time.Minute
)

func (m *Manager) watchFolder(ctx context.Context, acct mail.Account, r folderRef, kick chan<- struct{}) {
	failures := 0
	for {
		started := time.Now()
		err := m.watchSession(ctx, acct, r, kick)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("sync account %d folder %s watch: %v", acct.ID, r.name, err)
		}
		if time.Since(started) >= watchRetryCap {
			failures = 0
		}
		delay := watchRetryBase
		for i := 0; i < failures && delay < watchRetryCap; i++ {
			delay *= 2
		}
		if delay > watchRetryCap {
			delay = watchRetryCap
		}
		failures++
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
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
		poll := time.NewTimer(60 * time.Second)
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
	defer func() {
		// Rows may have been wiped and relisted under new ids during this
		// pass. Ops bind messages by durable identity, so nothing about
		// them changes here; only their cached row coordinates and this
		// folder's unread counter are refreshed before the change is
		// broadcast, keeping both honest against the new incarnations.
		if _, err := m.st.RefreshOpHints(acct.ID); err != nil {
			log.Printf("sync account %d: refresh op hints: %v", acct.ID, err)
		}
		if err := m.st.RecomputeFolderUnread(folderID); err != nil {
			log.Printf("sync account %d: recompute unread for folder %d: %v", acct.ID, folderID, err)
		}
		m.emit(change)
	}()
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

	startModSeq := state.HighestModSeq

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
				row := m.messageRow(acct, msg, 0)
				id, inserted, err := m.st.UpsertMessage(acct.ID, folderID, row)
				if err != nil {
					return change, fmt.Errorf("upsert message: %w", err)
				}
				if inserted {
					change.Inserted = append(change.Inserted, id)
					if row.ThreadKey != "" && msg.MessageID != "" {
						_ = m.st.AdoptThreadKey(acct.ID, msg.MessageID, row.ThreadKey, id, msg.Subject)
					}
				}

			}
			start = end + 1
		}
	}

	var highestModSeq = startModSeq
	if cl.HasCap(imap.CapCondStore) && startModSeq > 0 && state.UIDNext > 0 {
		changed, err := cl.FetchFlags(startModSeq)
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
		if err := m.reconcileDeletions(acct.ID, folderID, seen, &change); err != nil {
			return change, err
		}
		if err := m.reconcileMissing(cl, acct, folderID, seen, &change); err != nil {
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
		if err := m.reconcileDeletions(acct.ID, folderID, seen, &change); err != nil {
			return change, err
		}
		if err := m.reconcileMissing(cl, acct, folderID, seen, &change); err != nil {
			return change, err
		}
	}

	if err := m.claimPendingTwins(acct.ID, folderID, &change); err != nil {
		return change, err
	}
	err = m.st.UpdateFolderSync(folderID, sel.UIDValidity, uidNext, highestModSeq, int(sel.NumMessages))
	return change, err
}

func (m *Manager) messageRow(acct mail.Account, msg mail.Msg, firstSeenAt int64) *store.MessageRow {
	return &store.MessageRow{
		UID:         msg.UID,
		MessageID:   msg.MessageID,
		ThreadKey:   m.st.ComputeThreadKey(acct.ID, msg.MessageID, msg.InReplyTo, msg.Subject),
		Subject:     msg.Subject,
		FromJSON:    addrsJSON(msg.From),
		ToJSON:      addrsJSON(msg.To),
		CcJSON:      addrsJSON(msg.Cc),
		DateTS:      msg.Date.Unix(),
		Flags:       msg.Flags,
		HasAttach:   msg.HasAttach,
		Size:        msg.Size,
		ModSeq:      msg.ModSeq,
		FirstSeenAt: firstSeenAt,
	}
}

func (m *Manager) reconcileMissing(cl mail.Conn, acct mail.Account, folderID int64, seen map[uint32]bool, change *Change) error {
	local, err := m.st.FolderUIDs(folderID)
	if err != nil {
		return err
	}
	claimed, err := m.st.ActiveOpPriorUIDs(acct.ID, folderID)
	if err != nil {
		return err
	}
	missing := make([]uint32, 0)
	for uid := range seen {
		if !local[uid] && !claimed[uid] {
			missing = append(missing, uid)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	for start := 0; start < len(missing); start += 200 {
		end := start + 200
		if end > len(missing) {
			end = len(missing)
		}
		msgs, err := cl.FetchEnvelopes(missing[start:end])
		if err != nil {
			return fmt.Errorf("fetch missing envelopes: %w", err)
		}
		for _, msg := range msgs {
			row := m.messageRow(acct, msg, msg.Date.Unix())
			id, inserted, err := m.st.UpsertMessage(acct.ID, folderID, row)
			if err != nil {
				return fmt.Errorf("upsert missing message: %w", err)
			}
			if inserted {
				change.Inserted = append(change.Inserted, id)
				if row.ThreadKey != "" && msg.MessageID != "" {
					_ = m.st.AdoptThreadKey(acct.ID, msg.MessageID, row.ThreadKey, id, msg.Subject)
				}
			}
		}
	}
	return nil
}

func (m *Manager) reconcileDeletions(accountID, folderID int64, seen map[uint32]bool, change *Change) error {
	local, err := m.st.FolderUIDs(folderID)
	if err != nil {
		return err
	}
	pending, err := m.st.PendingUIDs(folderID)
	if err != nil {
		return err
	}
	for uid := range local {
		if seen[uid] {
			if pending[uid] {
				if id, ok := m.st.LocalID(folderID, uid); ok {
					if err := m.st.SetPendingDelete(id, false); err != nil {
						return err
					}
				}
			}
			continue
		}
		row, _, err := m.st.RowForClaim(folderID, uid)
		if err != nil {
			return err
		}
		if row.ID == 0 {
			continue
		}
		if active, err := m.st.MessageHasActiveOps(row.ID); err != nil {
			return err
		} else if active {
			continue
		}
		if row.MessageID != "" {
			if twin, found, err := m.st.TwinRow(accountID, row.MessageID, row.ID); err != nil {
				return err
			} else if found {
				claimed, err := m.claimRows(row, twin, change)
				if err != nil {
					return err
				}
				if claimed {
					continue
				}
			}
		}
		if !pending[uid] {
			if err := m.st.SetPendingDelete(row.ID, true); err != nil {
				return err
			}
			continue
		}
		change.Removed = append(change.Removed, row.ID)
		if err := m.st.DeleteMessage(folderID, uid); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) claimRows(orig, twin store.ClaimRow, change *Change) (bool, error) {
	survivor, loser := orig, twin
	if orig.BodyState == 0 && twin.BodyState != 0 {
		survivor, loser = twin, orig
	}
	done, err := m.st.ClaimMove(survivor.ID, loser.ID, twin.FolderID, twin.UID)
	if err != nil || !done {
		return false, err
	}
	if survivor.ID == orig.ID {
		change.Removed = append(change.Removed, orig.ID)
		change.CrossRemoved = append(change.CrossRemoved, CrossRemoval{FolderID: twin.FolderID, IDs: []int64{twin.ID}})
	} else {
		change.Removed = append(change.Removed, orig.ID)
	}
	change.Inserted = append(change.Inserted, survivor.ID)
	return true, nil
}

func (m *Manager) claimPendingTwins(accountID, folderID int64, change *Change) error {
	pendings, err := m.st.PendingRows(accountID)
	if err != nil {
		return err
	}
	claimedMIDs := make(map[string]bool)
	for _, p := range pendings {
		if p.FolderID == folderID || claimedMIDs[p.MessageID] {
			continue
		}
		twin, found, err := m.st.TwinInFolder(accountID, folderID, p.MessageID, p.ID)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		survivor, loser := p, twin
		if p.BodyState == 0 && twin.BodyState != 0 {
			survivor, loser = twin, p
		}
		done, err := m.st.ClaimMove(survivor.ID, loser.ID, twin.FolderID, twin.UID)
		if err != nil {
			return err
		}
		if !done {
			continue
		}
		claimedMIDs[p.MessageID] = true
		change.CrossRemoved = append(change.CrossRemoved, CrossRemoval{FolderID: p.FolderID, IDs: []int64{p.ID}})
		if survivor.ID == p.ID {
			swapped := false
			for i, id := range change.Inserted {
				if id == twin.ID {
					change.Inserted[i] = p.ID
					swapped = true
				}
			}
			if !swapped {
				change.CrossRemoved = append(change.CrossRemoved, CrossRemoval{FolderID: twin.FolderID, IDs: []int64{twin.ID}})
				change.Inserted = append(change.Inserted, p.ID)
			}
		} else {
			seen := false
			for _, id := range change.Inserted {
				if id == twin.ID {
					seen = true
				}
			}
			if !seen {
				change.Inserted = append(change.Inserted, twin.ID)
			}
		}
	}
	return nil
}
