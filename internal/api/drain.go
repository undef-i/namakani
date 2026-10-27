package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/textproto"
	"sync"
	"sync/atomic"
	"time"

	"namakani/internal/auth"
	"namakani/internal/mail"
	"namakani/internal/proton"
	"namakani/internal/store"
	"namakani/internal/tuta"

	"github.com/emersion/go-imap/v2"
)

type deleteBatcher interface {
	DeleteMessages(uids []uint32) error
}

func (s *Server) openDrainConn(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
	return s.openConn(key, accountID)
}

func (s *Server) anyCredentialKey() ([]byte, bool) {
	s.mu.Lock()
	for _, k := range s.keys {
		s.mu.Unlock()
		return k, true
	}
	s.mu.Unlock()
	if s.serverKey == nil {
		return nil, false
	}
	rows, err := s.st.DB.Query("SELECT wrapped_key, expires_at FROM sessions WHERE wrapped_key != '' ORDER BY expires_at DESC")
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	now := time.Now().Unix()
	for rows.Next() {
		var wrapped string
		var expiresAt int64
		if err := rows.Scan(&wrapped, &expiresAt); err != nil || expiresAt <= now {
			continue
		}
		plain, err := auth.Decrypt(s.serverKey, wrapped)
		if err == nil {
			return []byte(plain), true
		}
	}
	return nil, false
}

func (s *Server) kickDrain() {
	select {
	case s.drainWake <- struct{}{}:
	default:
	}
}

func (s *Server) drainLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.drainWake:
		}
		s.drainSweep()
	}
}

func (s *Server) drainSweep() {
	key, ok := s.anyCredentialKey()
	if !ok {
		return
	}
	now := time.Now().Unix()
	accounts, err := s.st.AccountsWithDueOps(now)
	if err != nil {
		return
	}
	for _, accountID := range accounts {
		s.drainAccount(key, accountID)
	}
	_ = s.st.PurgeTerminalOps(now)
}

type connFailState struct {
	count int
	until int64
}

func (s *Server) connBackedOff(accountID int64, now int64) bool {
	s.connFailMu.Lock()
	defer s.connFailMu.Unlock()
	st := s.connFail[accountID]
	return st != nil && now < st.until
}

func (s *Server) noteConnFailure(accountID int64, now int64) {
	s.connFailMu.Lock()
	defer s.connFailMu.Unlock()
	st := s.connFail[accountID]
	if st == nil {
		st = &connFailState{}
		s.connFail[accountID] = st
	}
	st.count++
	st.until = now + store.BackoffSeconds(st.count)
}

func (s *Server) noteConnSuccess(accountID int64) {
	s.connFailMu.Lock()
	defer s.connFailMu.Unlock()
	delete(s.connFail, accountID)
}

type drainWave struct {
	token    int64
	progress atomic.Int64
	dead     atomic.Bool
	connMu   sync.Mutex
	conn     mail.Conn
}

func (w *drainWave) bump() {
	w.progress.Store(time.Now().Unix())
}

func (w *drainWave) setConn(c mail.Conn) {
	w.connMu.Lock()
	w.conn = c
	w.connMu.Unlock()
}

func (w *drainWave) takeConn() mail.Conn {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	c := w.conn
	w.conn = nil
	return c
}

func (s *Server) registerWave(accountID int64) (*drainWave, bool) {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if _, ok := s.drainWaves[accountID]; ok {
		return nil, false
	}
	w := &drainWave{token: s.drainTokenSeq.Add(1)}
	w.bump()
	s.drainWaves[accountID] = w
	return w, true
}

func (s *Server) unregisterWave(accountID int64, w *drainWave) {
	s.drainMu.Lock()
	if s.drainWaves[accountID] == w {
		delete(s.drainWaves, accountID)
	}
	s.drainMu.Unlock()
}

func (s *Server) stampClaims(w *drainWave, ops []*store.Op) {
	ids := make([]int64, len(ops))
	for i, o := range ops {
		ids[i] = o.ID
		o.ClaimToken = w.token
	}
	if err := s.st.StampClaimToken(ids, w.token); err != nil {
		log.Printf("namakani: drain: stamp claim token %d: %v", w.token, err)
	}
	w.bump()
}

func (s *Server) opStillOurs(o *store.Op) bool {
	cur, err := s.st.GetOp(o.ID)
	if err != nil {
		return false
	}
	return cur.Status != store.OpInFlight || cur.ClaimToken == o.ClaimToken
}

func (s *Server) claimWatchdog() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now().Unix()
		s.killStuckWaves(now)
		s.reclaimOrphanedClaims()
	}
}

func (s *Server) killStuckWaves(now int64) {
	type stuck struct {
		accountID int64
		wave      *drainWave
	}
	var dead []stuck
	s.drainMu.Lock()
	for accountID, w := range s.drainWaves {
		if now-w.progress.Load() > s.claimStuckSecs {
			delete(s.drainWaves, accountID)
			dead = append(dead, stuck{accountID, w})
		}
	}
	s.drainMu.Unlock()
	for _, d := range dead {
		d.wave.dead.Store(true)
		if c := d.wave.takeConn(); c != nil {
			c.Close()
		}
		log.Printf("namakani: drain account %d: wave %d made no progress for over %d seconds, retired; its claims return to pending", d.accountID, d.wave.token, s.claimStuckSecs)
	}
}

func (s *Server) reclaimOrphanedClaims() {
	accounts, err := s.st.AccountsWithInFlightOps()
	if err != nil {
		return
	}
	for _, accountID := range accounts {
		s.drainMu.Lock()
		_, owned := s.drainWaves[accountID]
		s.drainMu.Unlock()
		if owned {
			continue
		}
		if n, err := s.st.ReleaseInFlightOps(accountID); err != nil {
			log.Printf("namakani: drain account %d: reclaim orphaned in-flight ops: %v", accountID, err)
		} else if n > 0 {
			log.Printf("namakani: drain account %d: reclaimed %d orphaned in-flight ops", accountID, n)
			s.kickDrain()
		}
		if sealed, err := s.st.ListSealedInFlightSends(accountID); err == nil && len(sealed) > 0 {
			if key, ok := s.anyCredentialKey(); ok {
				s.settleSealedSends(key, accountID)
			}
		}
	}
}

func (s *Server) settleSealedSends(key []byte, accountID int64) {
	orphans, err := s.st.ListSealedInFlightSends(accountID)
	if err != nil {
		log.Printf("namakani: drain account %d: list sealed in-flight sends: %v", accountID, err)
		return
	}
	for _, o := range orphans {
		newToken := s.drainTokenSeq.Add(1)
		adopted, err := s.st.AdoptSealedSend(o.ID, o.ClaimToken, newToken)
		if err != nil || !adopted {
			continue
		}
		o.ClaimToken = newToken
		var sp sendOpPayload
		_ = json.Unmarshal([]byte(o.Payload), &sp)
		if sp.Sent {
			s.executeSend(key, o)
			continue
		}
		parked, err := s.st.ParkSealedSendUnknown(o.ID)
		if err != nil || !parked {
			continue
		}
		log.Printf("namakani: drain account %d: send op %d was sealed by a dead wave and its delivery outcome is unknown; parked for attention", accountID, o.ID)
		s.broadcastSendState(o.ID, store.OpNeedsAttention, false, false)
		s.broadcastOps()
		s.broadcastScheduled()
	}
}

func (s *Server) drainAccount(key []byte, accountID int64) {
	wave, ok := s.registerWave(accountID)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("namakani: drain account %d: wave %d panicked, its claims are left for the reaper: %v", accountID, wave.token, r)
		}
		s.unregisterWave(accountID, wave)
	}()
	if n, err := s.st.ReleaseInFlightOps(accountID); err != nil {
		log.Printf("namakani: drain account %d: release stranded in-flight ops: %v", accountID, err)
	} else if n > 0 {
		log.Printf("namakani: drain account %d: released %d stranded in-flight ops", accountID, n)
	}
	s.settleSealedSends(key, accountID)
	var runFailed []int64
	for {
		if wave.dead.Load() {
			return
		}
		progressed := false
		sends, err := s.st.ClaimDueOps(accountID, time.Now().Unix(), 500, store.OpSend)
		if err != nil {
			return
		}
		if len(sends) > 0 {
			s.stampClaims(wave, sends)
			for _, o := range sends {
				s.broadcastSendState(o.ID, store.OpInFlight, false, false)
			}
			done, failed := s.executeSendWave(key, wave, sends)
			progressed = done > 0 || len(failed) > 0
			runFailed = append(runFailed, failed...)
		}
		if s.connBackedOff(accountID, time.Now().Unix()) {
			if progressed {
				_ = s.st.AccelerateAccountOps(accountID, time.Now().Unix(), runFailed)
				continue
			}
			return
		}
		restDue, err := s.st.HasDueOps(accountID, time.Now().Unix(), store.OpMove, store.OpFlags, store.OpExpunge)
		if err != nil || !restDue {
			if progressed {
				_ = s.st.AccelerateAccountOps(accountID, time.Now().Unix(), runFailed)
				continue
			}
			return
		}
		conn, err := s.drainOpen(context.Background(), key, accountID)
		if err != nil {
			s.noteConnFailure(accountID, time.Now().Unix())
			log.Printf("namakani: drain account %d: connect: %v", accountID, err)
			return
		}
		wave.setConn(conn)
		s.noteConnSuccess(accountID)
		wave.bump()
		rest, err := s.st.ClaimDueOps(accountID, time.Now().Unix(), 500, store.OpMove, store.OpFlags, store.OpExpunge)
		if err != nil || len(rest) == 0 {
			conn.Close()
			wave.setConn(nil)
			if progressed {
				_ = s.st.AccelerateAccountOps(accountID, time.Now().Unix(), runFailed)
				continue
			}
			return
		}
		s.stampClaims(wave, rest)
		done, failed := s.executeWave(conn, wave, accountID, rest)
		progressed = progressed || done > 0 || len(failed) > 0
		runFailed = append(runFailed, failed...)
		wave.takeConn()
		conn.Close()
		wave.bump()
		if !progressed {
			return
		}
		_ = s.st.AccelerateAccountOps(accountID, time.Now().Unix(), runFailed)
	}
}

type opOutcome struct {
	op         *store.Op
	gone       bool
	failed     error
	perm       bool
	destUID    uint32
	moveGone   bool
	target     store.ResolvedTarget
	hasTarget  bool
	targetLost bool
}

func classifyOpError(err error) (permanent, gone bool) {
	if err == nil {
		return false, false
	}
	if isNotInFolder(err) {
		return false, true
	}
	var ie *imap.Error
	if errors.As(err, &ie) && ie.Code == imap.ResponseCodeAuthenticationFailed {
		return true, false
	}
	var re *tuta.RestError
	if errors.As(err, &re) {
		switch {
		case re.StatusCode == 401 || re.StatusCode == 403:
			return true, false
		case re.StatusCode == 429 || re.StatusCode >= 500:
			return false, false
		default:
			return true, false
		}
	}
	if term, _ := proton.ClassifyError(err); term {
		return true, false
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		if te.Code >= 500 {
			return true, false
		}
		return false, false
	}
	return false, false
}

func (s *Server) executeWave(conn mail.Conn, wave *drainWave, accountID int64, ops []*store.Op) (int, []int64) {
	outcomes := make([]*opOutcome, len(ops))
	byID := make(map[int64]*opOutcome, len(ops))
	for i, o := range ops {
		outcomes[i] = &opOutcome{op: o}
		byID[o.ID] = outcomes[i]
	}
	var execOps []*store.Op
	for _, o := range ops {
		oc := byID[o.ID]
		if o.Type == store.OpSend {
			execOps = append(execOps, o)
			continue
		}
		var target store.ResolvedTarget
		var ok bool
		var err error
		if o.Type == store.OpMove {
			target, ok, err = s.st.ResolveMoveSource(o)
		} else {
			target, ok, err = s.st.ResolveOpTarget(o)
		}
		if err != nil {
			oc.failed = err
			oc.perm = true
			continue
		}
		if !ok {
			oc.targetLost = true
			continue
		}
		oc.target = target
		oc.hasTarget = true
		o.MessageID = target.RowID
		execOps = append(execOps, o)
	}
	wave.bump()
	s.execFlags(conn, wave, execOps, byID)
	wave.bump()
	s.execMoves(conn, wave, execOps, byID)
	wave.bump()
	s.execExpunges(conn, wave, execOps, byID)
	wave.bump()
	now := time.Now().Unix()
	parked := false
	var voidedUndo []int64
	failedSet := make(map[int64]bool)
	var failedIDs []int64
	fail := func(oc *opOutcome) {
		if !failedSet[oc.op.ID] {
			failedSet[oc.op.ID] = true
			failedIDs = append(failedIDs, oc.op.ID)
		}
		hadUndo := false
		if cur, err := s.st.GetOp(oc.op.ID); err == nil {
			hadUndo = cur.UndoRequested
		}
		status, err := s.st.FailOp(oc.op.ID, oc.failed.Error(), oc.perm, now)
		if err == nil && status == store.OpNeedsAttention {
			parked = true
			s.hub.broadcast(map[string]any{"type": "ops", "op_id": oc.op.ID, "status": store.OpNeedsAttention})
			if hadUndo {
				voidedUndo = append(voidedUndo, oc.op.ID)
			}
		}
	}
	done := 0
	lostLanded := false
	for _, oc := range outcomes {
		wave.bump()
		if !s.opStillOurs(oc.op) {
			continue
		}
		counted := false
		if oc.failed == nil || oc.gone {
			done++
			counted = true
		}
		landFail := func(err error) {
			if oc.failed == nil {
				oc.failed = err
			}
			fail(oc)
			if counted {
				done--
				counted = false
			}
		}
		o := oc.op
		switch o.Type {
		case store.OpFlags:
			if oc.targetLost {
				if err := s.st.CompleteOp(o.ID); err != nil {
					landFail(err)
				} else {
					lostLanded = true
				}
			} else if oc.gone {
				folderID := int64(0)
				if route, err := s.st.MessageRoute(o.MessageID); err == nil {
					folderID = route.FolderID
				}
				if err := s.st.DropMessageRow(o.MessageID); err != nil {
					landFail(err)
				} else if err := s.st.CompleteOp(o.ID); err != nil {
					landFail(err)
				} else if folderID != 0 {
					s.broadcastRemoved(folderID, []int64{o.MessageID})
					s.broadcastFolder(folderID)
				}
			} else if oc.failed != nil {
				fail(oc)
			} else if err := s.st.CompleteOp(o.ID); err != nil {
				landFail(err)
			} else if route, err := s.st.MessageRoute(o.MessageID); err == nil {
				s.broadcastFlags([]int64{o.MessageID})
				s.broadcastFolder(route.FolderID)
			}
		case store.OpMove:
			srcFolder := o.PriorFolderID
			if oc.hasTarget {
				srcFolder = oc.target.FolderID
			}
			if oc.targetLost {
				if err := s.st.CompleteOp(o.ID); err != nil {
					landFail(err)
				} else {
					lostLanded = true
				}
			} else if oc.failed != nil {
				fail(oc)
			} else if oc.moveGone {
				if err := s.st.DropMessageRow(o.MessageID); err != nil {
					landFail(err)
				} else if err := s.st.CompleteOp(o.ID); err != nil {
					landFail(err)
				} else {
					s.broadcastRemoved(srcFolder, []int64{o.MessageID})
					s.broadcastFolder(srcFolder)
					s.broadcastFolder(o.ToFolderID)
				}
			} else if err := s.st.CompleteMoveOp(o.ID, o.ToFolderID, o.MessageID, oc.destUID); err != nil {
				landFail(err)
			} else {
				s.broadcastRemoved(srcFolder, []int64{o.MessageID})
				s.broadcastFolder(srcFolder)
				s.broadcastFolder(o.ToFolderID)
				s.broadcastMessages([]int64{o.MessageID})
			}
		case store.OpExpunge:
			if oc.targetLost {
				if err := s.st.CompleteOp(o.ID); err != nil {
					landFail(err)
				} else {
					lostLanded = true
				}
			} else if oc.failed != nil && !oc.gone {
				fail(oc)
			} else if err := s.st.DeleteRowOnly(o.MessageID); err != nil {
				landFail(err)
			} else if err := s.st.CompleteOp(o.ID); err != nil {
				landFail(err)
			} else {
				s.broadcastRemoved(o.PriorFolderID, []int64{o.MessageID})
				s.broadcastFolder(o.PriorFolderID)
			}
		}
	}
	if parked || lostLanded {
		s.broadcastOps()
	}
	for _, id := range voidedUndo {
		s.hub.broadcast(map[string]any{"type": "ops", "op_id": id, "undo_failed": true, "error": "这条操作在撤销执行前已经失败"})
	}
	for _, oc := range outcomes {
		if oc.failed != nil && !oc.gone {
			continue
		}
		cur, err := s.st.GetOp(oc.op.ID)
		if err != nil || !cur.UndoRequested || cur.Status != store.OpDone || cur.ClaimToken != oc.op.ClaimToken {
			continue
		}
		res, err := s.st.UndoOp(cur.ID)
		if err != nil {
			log.Printf("namakani: deferred undo op %d: %v", cur.ID, err)
			msg, ok := undoErrText(cur, err)
			if !ok {
				msg = "撤销失败"
			}
			_ = s.st.RecordUndoFailure(cur.ID, msg)
			s.hub.broadcast(map[string]any{"type": "ops", "op_id": cur.ID, "undo_failed": true, "error": msg})
			s.broadcastOps()
			continue
		}
		if res != nil && res.Inverse != nil {
			s.broadcastOpApplied(res.Inverse)
		}
	}
	return done, failedIDs
}

func (s *Server) broadcastOps() {
	n, err := s.st.AttentionCount()
	if err != nil {
		return
	}
	s.hub.broadcast(map[string]any{"type": "ops", "attention": n})
}

type flagPart struct {
	op    *store.Op
	uid   uint32
	flag  imap.Flag
	bit   uint32
	add   bool
	opRef *opOutcome
}

func (s *Server) execFlags(conn mail.Conn, wave *drainWave, ops []*store.Op, byID map[int64]*opOutcome) {
	type groupKey struct {
		folder string
		flag   imap.Flag
		add    bool
	}
	groups := map[groupKey][]*flagPart{}
	var order []groupKey
	partsLeft := map[int64]int{}
	selected := ""
	selectFailed := map[string]error{}
	for _, o := range ops {
		if o.Type != store.OpFlags {
			continue
		}
		oc := byID[o.ID]
		route := oc.target
		var payload store.FlagsPayload
		if err := json.Unmarshal([]byte(o.Payload), &payload); err != nil {
			oc.failed = err
			oc.perm = true
			continue
		}
		var parts []*flagPart
		if payload.Seen != nil {
			parts = append(parts, &flagPart{op: o, uid: route.UID, flag: imap.FlagSeen, bit: mail.FlagSeen, add: *payload.Seen, opRef: oc})
		}
		if payload.Flagged != nil {
			parts = append(parts, &flagPart{op: o, uid: route.UID, flag: imap.FlagFlagged, bit: mail.FlagFlagged, add: *payload.Flagged, opRef: oc})
		}
		if len(parts) == 0 {
			continue
		}
		partsLeft[o.ID] = len(parts)
		for _, p := range parts {
			k := groupKey{folder: route.FolderName, flag: p.flag, add: p.add}
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], p)
		}
	}
	for _, k := range order {
		parts := groups[k]
		wave.bump()
		if selected != k.folder {
			if _, err := conn.SelectRW(k.folder); err != nil {
				selectFailed[k.folder] = err
				for _, p := range parts {
					if p.opRef.failed == nil {
						p.opRef.failed = err
						p.opRef.perm, _ = classifyOpError(err)
					}
				}
				continue
			}
			selected = k.folder
		}
		uids := make([]uint32, len(parts))
		for i, p := range parts {
			uids[i] = p.uid
		}
		err := conn.StoreFlags(uids, k.flag, k.add)
		if err != nil {
			perm, gone := classifyOpError(err)
			if gone {
				s.isolateGoneFlags(conn, parts, partsLeft)
				continue
			}
			for _, p := range parts {
				if p.opRef.failed == nil {
					p.opRef.failed = err
					p.opRef.perm = perm
				}
			}
			continue
		}
		for _, p := range parts {
			partsLeft[p.op.ID]--
		}
	}
	for _, o := range ops {
		if o.Type != store.OpFlags {
			continue
		}
		oc := byID[o.ID]
		if oc.failed == nil && partsLeft[o.ID] > 0 {
			oc.failed = errors.New("flag store incomplete")
		}
	}
}

func (s *Server) isolateGoneFlags(conn mail.Conn, parts []*flagPart, partsLeft map[int64]int) {
	seen := map[int64]bool{}
	for _, p := range parts {
		err := conn.StoreFlag(p.uid, p.flag, p.add)
		if err == nil {
			partsLeft[p.op.ID]--
			continue
		}
		if _, gone := classifyOpError(err); gone {
			if !seen[p.op.ID] {
				seen[p.op.ID] = true
				p.opRef.gone = true
				p.opRef.failed = nil
			}
			continue
		}
		if p.opRef.failed == nil {
			p.opRef.failed = err
			p.opRef.perm, _ = classifyOpError(err)
		}
	}
}

func (s *Server) execMoves(conn mail.Conn, wave *drainWave, ops []*store.Op, byID map[int64]*opOutcome) {
	for _, o := range ops {
		if o.Type != store.OpMove {
			continue
		}
		s.execMoveOne(conn, o, byID[o.ID])
		wave.bump()
	}
}

func (s *Server) execMoveOne(conn mail.Conn, o *store.Op, oc *opOutcome) {
	var payload store.MovePayload
	if err := json.Unmarshal([]byte(o.Payload), &payload); err != nil {
		oc.failed = err
		oc.perm = true
		return
	}
	srcName := oc.target.FolderName
	if payload.ProbeUIDNext == nil || payload.ProbeUIDValidity != nil {
		if sel, serr := conn.Select(payload.ToFolder); serr == nil {
			if payload.ProbeUIDNext == nil || (payload.ProbeUIDValidity != nil && sel.UIDValidity != *payload.ProbeUIDValidity) {
				probe := sel.UIDNext
				validity := sel.UIDValidity
				payload.ProbeUIDNext = &probe
				payload.ProbeUIDValidity = &validity
				raw, merr := json.Marshal(payload)
				if merr != nil {
					oc.failed = merr
					return
				}
				if _, uerr := s.st.UpdateOpPayload(o.ID, string(raw)); uerr != nil {
					oc.failed = uerr
					return
				}
			}
		}
	}
	if _, err := conn.SelectRW(srcName); err != nil {
		oc.failed = err
		oc.perm, _ = classifyOpError(err)
		return
	}
	destUID, err := conn.MoveMessage(oc.target.UID, payload.ToFolder)
	if err != nil {
		perm, gone := classifyOpError(err)
		if !gone {
			oc.failed = err
			oc.perm = perm
			return
		}
		if uid, ok := s.resolveMovedUID(conn, o, payload); ok {
			oc.destUID = uid
			return
		}
		if uid, ok := s.st.MovedTwinUID(o.AccountID, payload.ToFolderID, o.MessageID); ok {
			oc.destUID = uid
			return
		}
		if payload.ProbeUIDNext == nil {
			oc.failed = errors.New("move target state unproven: probe unavailable")
			oc.perm = true
			return
		}
		oc.moveGone = true
		return
	}
	if destUID == 0 {
		destUID = s.findMovedUID(conn, o.MessageID, payload.ToFolder)
	}
	if destUID == 0 {
		if uid, ok := s.resolveMovedUID(conn, o, payload); ok {
			destUID = uid
		}
	}
	if destUID == 0 {
		if uid, ok := s.st.MovedTwinUID(o.AccountID, payload.ToFolderID, o.MessageID); ok {
			destUID = uid
		}
	}
	if destUID == 0 {
		oc.failed = errors.New("provider moved the message but its destination uid could not be determined")
		return
	}
	oc.destUID = destUID
}

func (s *Server) resolveMovedUID(conn mail.Conn, o *store.Op, payload store.MovePayload) (uint32, bool) {
	if payload.ProbeUIDNext == nil {
		return 0, false
	}
	lister, ok := conn.(mail.UIDLister)
	if !ok {
		return 0, false
	}
	sel, err := conn.Select(payload.ToFolder)
	if err != nil {
		return 0, false
	}
	if payload.ProbeUIDValidity != nil && sel.UIDValidity != *payload.ProbeUIDValidity {
		return 0, false
	}
	uids, err := lister.FetchUIDs()
	if err != nil {
		return 0, false
	}
	var cand []uint32
	for _, u := range uids {
		if u >= *payload.ProbeUIDNext {
			cand = append(cand, u)
		}
	}
	if len(cand) == 1 {
		return cand[0], true
	}
	if len(cand) > 1 {
		if uid := s.findMovedUID(conn, o.MessageID, payload.ToFolder); uid != 0 {
			return uid, true
		}
	}
	return 0, false
}

func (s *Server) execExpunges(conn mail.Conn, wave *drainWave, ops []*store.Op, byID map[int64]*opOutcome) {
	type group struct {
		folder string
		parts  []*opOutcome
		uids   []uint32
		byUID  map[uint32]*opOutcome
	}
	groups := map[string]*group{}
	var order []string
	selected := ""
	for _, o := range ops {
		if o.Type != store.OpExpunge {
			continue
		}
		oc := byID[o.ID]
		route := oc.target
		g, ok := groups[route.FolderName]
		if !ok {
			g = &group{folder: route.FolderName, byUID: map[uint32]*opOutcome{}}
			groups[route.FolderName] = g
			order = append(order, route.FolderName)
		}
		g.parts = append(g.parts, oc)
		g.uids = append(g.uids, route.UID)
		g.byUID[route.UID] = oc
	}
	for _, name := range order {
		g := groups[name]
		wave.bump()
		if selected != name {
			if _, err := conn.SelectRW(name); err != nil {
				for _, oc := range g.parts {
					oc.failed = err
					oc.perm, _ = classifyOpError(err)
				}
				continue
			}
			selected = name
		}
		if b, ok := conn.(deleteBatcher); ok {
			err := b.DeleteMessages(g.uids)
			if err != nil {
				perm, gone := classifyOpError(err)
				if gone {
					for _, oc := range g.parts {
						oc.gone = true
					}
					continue
				}
				for _, oc := range g.parts {
					oc.failed = err
					oc.perm = perm
				}
			}
			continue
		}
		for _, oc := range g.parts {
			if err := conn.DeleteMessage(oc.target.UID); err != nil {
				perm, gone := classifyOpError(err)
				if gone {
					oc.gone = true
					continue
				}
				oc.failed = err
				oc.perm = perm
			}
		}
	}
}

func (s *Server) folderName(folderID int64) (string, error) {
	var name string
	err := s.st.DB.QueryRow("SELECT name FROM folders WHERE id = ?", folderID).Scan(&name)
	return name, err
}
