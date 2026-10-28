package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

const (
	OpFlags   = "flags"
	OpMove    = "move"
	OpExpunge = "expunge"
	OpSend    = "send"
)

const (
	OpPending        = "pending"
	OpInFlight       = "in_flight"
	OpDone           = "done"
	OpNeedsAttention = "needs_attention"
	OpCancelled      = "cancelled"
)

const provisionalUIDBase = 1 << 31

const maxOpAttempts = 8

var (
	ErrOpNotFound   = errors.New("op not found")
	ErrOpNotActive  = errors.New("op not active")
	ErrOpSuperseded = errors.New("op superseded by a newer op")
	ErrOpCannotUndo = errors.New("op cannot be undone")
	ErrOpBlocked    = errors.New("message has an active op")
	ErrUndoTargetGone = errors.New("undo target no longer exists")
)

type Op struct {
	ID            int64
	AccountID     int64
	Type          string
	MessageID     int64
	Payload       string
	PriorFolderID int64
	PriorUID      uint32
	PriorFlags    uint32
	PriorUIDValidity uint32
	ToFolderID    int64
	NotBefore     int64
	Status        string
	Attempts      int
	NextAttempt   int64
	LastError     string
	ClientKey     string
	UndoRequested bool
	CreatedAt     int64
	Identity      string
	ClaimToken    int64
	Sealed        bool
	ReturnToDraft bool
}

type FlagsPayload struct {
	Seen    *bool `json:"seen,omitempty"`
	Flagged *bool `json:"flagged,omitempty"`
}

type MovePayload struct {
	ToFolderID       int64   `json:"to_folder_id"`
	ToFolder         string  `json:"to_folder"`
	ProbeUIDNext     *uint32 `json:"probe_uidnext,omitempty"`
	ProbeUIDValidity *uint32 `json:"probe_uidvalidity,omitempty"`
}

const opColumns = `id, account_id, type, message_id, payload, prior_folder_id, prior_uid, prior_flags,
	to_folder_id, not_before, status, attempts, next_attempt, last_error, client_key, created_at, undo_requested, prior_uidvalidity, identity, claim_token, sealed, return_to_draft`

func scanOp(row interface{ Scan(...any) error }) (*Op, error) {
	var o Op
	var priorUID, priorFlags, undoRequested, priorUIDValidity, sealed, returnToDraft int64
	err := row.Scan(&o.ID, &o.AccountID, &o.Type, &o.MessageID, &o.Payload,
		&o.PriorFolderID, &priorUID, &priorFlags, &o.ToFolderID, &o.NotBefore,
		&o.Status, &o.Attempts, &o.NextAttempt, &o.LastError, &o.ClientKey, &o.CreatedAt, &undoRequested, &priorUIDValidity, &o.Identity, &o.ClaimToken, &sealed, &returnToDraft)
	if err != nil {
		return nil, err
	}
	o.PriorUID = uint32(priorUID)
	o.PriorFlags = uint32(priorFlags)
	o.UndoRequested = undoRequested != 0
	o.PriorUIDValidity = uint32(priorUIDValidity)
	o.Sealed = sealed != 0
	o.ReturnToDraft = returnToDraft != 0
	return &o, nil
}

func folderValidityTx(tx *sql.Tx, folderID int64) (uint32, error) {
	var v int64
	if err := tx.QueryRow("SELECT uidvalidity FROM folders WHERE id = ?", folderID).Scan(&v); err != nil {
		return 0, err
	}
	return uint32(v), nil
}

func (s *Store) GetOp(id int64) (*Op, error) {
	o, err := scanOp(s.DB.QueryRow("SELECT "+opColumns+" FROM ops WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOpNotFound
	}
	return o, err
}

var ErrClientKeyConflict = errors.New("client key already used for a different operation")

func keyReuseErr(prev *Op, accountID int64, typ string, messageID int64, identity string) error {
	if prev.AccountID != accountID || prev.Type != typ {
		return ErrClientKeyConflict
	}
	if prev.Identity != "" && identity != "" {
		if prev.Identity != identity {
			return ErrClientKeyConflict
		}
		return nil
	}
	if prev.MessageID != messageID {
		return ErrClientKeyConflict
	}
	return nil
}

func flagsPayloadEqual(a, b FlagsPayload) bool {
	eq := func(x, y *bool) bool {
		if x == nil || y == nil {
			return x == y
		}
		return *x == *y
	}
	return eq(a.Seen, b.Seen) && eq(a.Flagged, b.Flagged)
}

func (s *Store) OpByClientKey(key string) (*Op, error) {
	o, err := scanOp(s.DB.QueryRow("SELECT "+opColumns+" FROM ops WHERE client_key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return s.opByKeyTombstone(nil, key)
	}
	return o, err
}

func (s *Store) opByClientKey(tx *sql.Tx, key string) (*Op, error) {
	o, err := scanOp(tx.QueryRow("SELECT "+opColumns+" FROM ops WHERE client_key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return o, err
}

func clientKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *Store) opByKeyTombstone(tx *sql.Tx, key string) (*Op, error) {
	if key == "" {
		return nil, nil
	}
	q := s.DB.QueryRow
	if tx != nil {
		q = tx.QueryRow
	}
	var o Op
	err := q(`SELECT op_id, account_id, type, message_id, to_folder_id, payload, status, created_at
		FROM op_client_keys WHERE key_hash = ?`, clientKeyHash(key)).Scan(
		&o.ID, &o.AccountID, &o.Type, &o.MessageID, &o.ToFolderID, &o.Payload, &o.Status, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.ClientKey = key
	return &o, nil
}

func (s *Store) keyPrior(tx *sql.Tx, key string) (*Op, error) {
	if key == "" {
		return nil, nil
	}
	o, err := s.opByClientKey(tx, key)
	if err != nil || o != nil {
		return o, err
	}
	return s.opByKeyTombstone(tx, key)
}

func insertKeyTombstone(tx *sql.Tx, key string, opID, accountID int64, typ string, messageID, toFolderID int64, payload string) error {
	if key == "" {
		return nil
	}
	now := time.Now().Unix()
	status := ""
	var expires int64
	if opID == 0 {
		status = OpCancelled
		expires = now + keyRetentionSecs
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO op_client_keys(key_hash, op_id, account_id, type, message_id,
		to_folder_id, payload, status, created_at, expires_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		clientKeyHash(key), opID, accountID, typ, messageID, toFolderID, payload, status, now, expires)
	return err
}

func (s *Store) activeOpsForIdentity(tx *sql.Tx, accountID int64, identity string) ([]*Op, error) {
	if identity == "" {
		return nil, nil
	}
	rows, err := tx.Query("SELECT "+opColumns+` FROM ops WHERE account_id = ? AND identity = ?
		AND status IN ('pending', 'in_flight', 'needs_attention') ORDER BY id`, accountID, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) activeOpsForMessage(tx *sql.Tx, messageID int64) ([]*Op, error) {
	rows, err := tx.Query("SELECT "+opColumns+` FROM ops WHERE message_id = ?
		AND status IN ('pending', 'in_flight', 'needs_attention') ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func refreshUnreadTx(tx *sql.Tx, folderID int64) error {
	_, err := tx.Exec(`UPDATE folders SET unread =
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = ? AND (m.flags & 1) = 0
			AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id AND o.type = 'expunge'
				AND o.status IN ('pending', 'in_flight', 'needs_attention'))) WHERE id = ?`,
		folderID, folderID)
	return err
}

func applyFlagsValue(cur uint32, p FlagsPayload) uint32 {
	if p.Seen != nil {
		if *p.Seen {
			cur |= 1
		} else {
			cur &^= 1
		}
	}
	if p.Flagged != nil {
		if *p.Flagged {
			cur |= 2
		} else {
			cur &^= 2
		}
	}
	return cur
}

func (s *Store) EnqueueFlags(accountID, messageID int64, seen, flagged *bool, clientKey string) (int64, uint32, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	id, flags, err := s.enqueueFlagsTx(tx, accountID, messageID, seen, flagged, clientKey)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return id, flags, nil
}

func (s *Store) enqueueFlagsTx(tx *sql.Tx, accountID, messageID int64, seen, flagged *bool, clientKey string) (int64, uint32, error) {
	var acctID, folderID int64
	var curFlags uint32
	if err := tx.QueryRow("SELECT account_id, folder_id, flags FROM messages WHERE id = ?", messageID).
		Scan(&acctID, &folderID, &curFlags); err != nil {
		return 0, 0, err
	}
	if acctID != accountID {
		return 0, 0, sql.ErrNoRows
	}
	identity, ierr := identityForRowTx(tx, accountID, messageID)
	if ierr != nil {
		return 0, 0, ierr
	}
	want := FlagsPayload{Seen: seen, Flagged: flagged}
	if clientKey != "" {
		if prev, err := s.keyPrior(tx, clientKey); err != nil {
			return 0, 0, err
		} else if prev != nil {
			if err := keyReuseErr(prev, accountID, OpFlags, messageID, identity); err != nil {
				return 0, 0, err
			}
			var prevWant FlagsPayload
			if err := json.Unmarshal([]byte(prev.Payload), &prevWant); err != nil || !flagsPayloadEqual(prevWant, want) {
				return 0, 0, ErrClientKeyConflict
			}
			return prev.ID, curFlags, nil
		}
	}
	active, err := s.activeOpsForMessage(tx, messageID)
	if err != nil {
		return 0, 0, err
	}
	if n := len(active); n > 0 {
		tail := active[n-1]
		if tail.Type == OpFlags && tail.Status == OpPending {
			var merged FlagsPayload
			if err := json.Unmarshal([]byte(tail.Payload), &merged); err != nil {
				return 0, 0, err
			}
			if want.Seen != nil {
				merged.Seen = want.Seen
			}
			if want.Flagged != nil {
				merged.Flagged = want.Flagged
			}
			newFlags := applyFlagsValue(tail.PriorFlags, merged)
			wantRaw, _ := json.Marshal(want)
			if newFlags == tail.PriorFlags {
				if _, err := tx.Exec("UPDATE ops SET status = 'cancelled', finished_at = unixepoch() WHERE id = ?", tail.ID); err != nil {
					return 0, 0, err
				}
				if _, err := tx.Exec("UPDATE messages SET flags = ? WHERE id = ?", tail.PriorFlags, messageID); err != nil {
					return 0, 0, err
				}
				if err := refreshUnreadTx(tx, folderID); err != nil {
					return 0, 0, err
				}
				if err := insertKeyTombstone(tx, clientKey, 0, accountID, OpFlags, messageID, 0, string(wantRaw)); err != nil {
					return 0, 0, err
				}
				return 0, tail.PriorFlags, nil
			}
			raw, _ := json.Marshal(merged)
			if _, err := tx.Exec("UPDATE ops SET payload = ? WHERE id = ?", string(raw), tail.ID); err != nil {
				return 0, 0, err
			}
			if _, err := tx.Exec("UPDATE messages SET flags = ? WHERE id = ?", newFlags, messageID); err != nil {
				return 0, 0, err
			}
			if err := refreshUnreadTx(tx, folderID); err != nil {
				return 0, 0, err
			}
			if err := insertKeyTombstone(tx, clientKey, tail.ID, accountID, OpFlags, messageID, 0, string(wantRaw)); err != nil {
				return 0, 0, err
			}
			return tail.ID, newFlags, nil
		}
	}
	newFlags := applyFlagsValue(curFlags, want)
	raw, _ := json.Marshal(want)
	validity, verr := folderValidityTx(tx, folderID)
	if verr != nil {
		return 0, 0, verr
	}
	res, err := tx.Exec(`INSERT INTO ops(account_id, type, message_id, payload, prior_folder_id, prior_flags, prior_uidvalidity, identity, status, client_key, created_at)
		VALUES(?, 'flags', ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		accountID, messageID, string(raw), folderID, curFlags, validity, identity, clientKey, time.Now().Unix())
	if err != nil {
		return 0, 0, err
	}
	opID, _ := res.LastInsertId()
	if err := insertKeyTombstone(tx, clientKey, opID, accountID, OpFlags, messageID, 0, string(raw)); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec("UPDATE messages SET flags = ? WHERE id = ?", newFlags, messageID); err != nil {
		return 0, 0, err
	}
	if err := refreshUnreadTx(tx, folderID); err != nil {
		return 0, 0, err
	}
	return opID, newFlags, nil
}

func (s *Store) EnqueueMove(accountID, messageID, destFolderID int64, clientKey string) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := s.enqueueMoveTx(tx, accountID, messageID, destFolderID, clientKey)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func noteFolderChange(db execQuerier, messageID, fromID, toID int64) error {
	if fromID == toID {
		return nil
	}
	var toSpecial string
	if err := db.QueryRow("SELECT COALESCE(special_use, '') FROM folders WHERE id = ?", toID).Scan(&toSpecial); err != nil {
		return err
	}
	if toSpecial == "trash" {
		_, err := db.Exec("UPDATE messages SET trash_origin_folder_id = ? WHERE id = ?", fromID, messageID)
		return err
	}
	var fromSpecial string
	if err := db.QueryRow("SELECT COALESCE(special_use, '') FROM folders WHERE id = ?", fromID).Scan(&fromSpecial); err != nil {
		return err
	}
	if fromSpecial == "trash" {
		_, err := db.Exec("UPDATE messages SET trash_origin_folder_id = 0 WHERE id = ?", messageID)
		return err
	}
	return nil
}

func (s *Store) ResolveRestoreFolder(accountID, messageID int64) (int64, error) {
	var flags uint32
	var origin, curFolder int64
	if err := s.DB.QueryRow("SELECT flags, trash_origin_folder_id, folder_id FROM messages WHERE id = ? AND account_id = ?",
		messageID, accountID).Scan(&flags, &origin, &curFolder); err != nil {
		return 0, err
	}
	var kind string
	if err := s.DB.QueryRow("SELECT kind FROM accounts WHERE id = ?", accountID).Scan(&kind); err != nil {
		return 0, err
	}
	byOrigin := func() (int64, bool, error) {
		if origin <= 0 || origin == curFolder {
			return 0, false, nil
		}
		var n int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM folders WHERE id = ? AND account_id = ?", origin, accountID).Scan(&n); err != nil {
			return 0, false, err
		}
		if n > 0 {
			return origin, true, nil
		}
		return 0, false, nil
	}
	byDraftBit := func() (int64, bool, error) {
		if flags&16 == 0 {
			return 0, false, nil
		}
		id, _, ok, err := s.FolderBySpecial(accountID, "drafts")
		if err != nil || !ok || id == curFolder {
			return 0, false, err
		}
		return id, true, nil
	}
	steps := []func() (int64, bool, error){byDraftBit, byOrigin}
	if kind == "tuta" {
		steps = []func() (int64, bool, error){byOrigin, byDraftBit}
	}
	for _, step := range steps {
		id, ok, err := step()
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
	}
	id, _, ok, err := s.FolderBySpecial(accountID, "inbox")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, sql.ErrNoRows
	}
	return id, nil
}

func (s *Store) enqueueMoveTx(tx *sql.Tx, accountID, messageID, destFolderID int64, clientKey string) (int64, error) {
	var acctID, folderID int64
	var uid uint32
	if err := tx.QueryRow("SELECT account_id, folder_id, uid FROM messages WHERE id = ?", messageID).
		Scan(&acctID, &folderID, &uid); err != nil {
		return 0, err
	}
	if acctID != accountID {
		return 0, sql.ErrNoRows
	}
	identity, ierr := identityForRowTx(tx, accountID, messageID)
	if ierr != nil {
		return 0, ierr
	}
	var destName string
	if err := tx.QueryRow("SELECT name FROM folders WHERE id = ? AND account_id = ?",
		destFolderID, accountID).Scan(&destName); err != nil {
		return 0, err
	}
	if folderID == destFolderID {
		return 0, nil
	}
	if clientKey != "" {
		if prev, err := s.keyPrior(tx, clientKey); err != nil {
			return 0, err
		} else if prev != nil {
			if err := keyReuseErr(prev, accountID, OpMove, messageID, identity); err != nil {
				return 0, err
			}
			if prev.PayloadFolderID() != destFolderID {
				return 0, ErrClientKeyConflict
			}
			return prev.ID, nil
		}
	}
	active, err := s.activeOpsForMessage(tx, messageID)
	if err != nil {
		return 0, err
	}
	for _, o := range active {
		if o.Type == OpExpunge {
			return 0, ErrOpBlocked
		}
	}
	idActive, err := s.activeOpsForIdentity(tx, accountID, identity)
	if err != nil {
		return 0, err
	}
	for _, o := range idActive {
		if o.Type == OpExpunge {
			return 0, ErrOpBlocked
		}
	}
	if n := len(active); n > 0 {
		tail := active[n-1]
		if tail.Type == OpMove && tail.Status == OpPending {
			oldDest := tail.PayloadFolderID()
			if tail.ToFolderID == destFolderID {
				raw, _ := json.Marshal(MovePayload{ToFolderID: destFolderID, ToFolder: destName})
				if err := insertKeyTombstone(tx, clientKey, tail.ID, accountID, OpMove, messageID, destFolderID, string(raw)); err != nil {
					return 0, err
				}
				return tail.ID, nil
			}
			if destFolderID == tail.PriorFolderID {
				if _, err := tx.Exec("UPDATE messages SET folder_id = ?, uid = ? WHERE id = ?",
					tail.PriorFolderID, tail.PriorUID, messageID); err != nil {
					return 0, err
				}
				if err := noteFolderChange(tx, messageID, oldDest, tail.PriorFolderID); err != nil {
					return 0, err
				}
				if err := adjustFolderTotalsTx(tx, oldDest, -1); err != nil {
					return 0, err
				}
				if err := adjustFolderTotalsTx(tx, tail.PriorFolderID, 1); err != nil {
					return 0, err
				}
				if _, err := tx.Exec("UPDATE ops SET status = 'cancelled', finished_at = unixepoch() WHERE id = ?", tail.ID); err != nil {
					return 0, err
				}
				if err := refreshUnreadTx(tx, oldDest); err != nil {
					return 0, err
				}
				if err := refreshUnreadTx(tx, tail.PriorFolderID); err != nil {
					return 0, err
				}
				raw, _ := json.Marshal(MovePayload{ToFolderID: destFolderID, ToFolder: destName})
				if err := insertKeyTombstone(tx, clientKey, 0, accountID, OpMove, messageID, destFolderID, string(raw)); err != nil {
					return 0, err
				}
				return 0, nil
			}
			var tailPayload MovePayload
			_ = json.Unmarshal([]byte(tail.Payload), &tailPayload)
			raw, _ := json.Marshal(MovePayload{ToFolderID: destFolderID, ToFolder: destName, ProbeUIDNext: tailPayload.ProbeUIDNext})
			if _, err := tx.Exec("UPDATE ops SET payload = ?, to_folder_id = ? WHERE id = ?",
				string(raw), destFolderID, tail.ID); err != nil {
				return 0, err
			}
			prov := provisionalUIDBase + tail.ID
			if _, err := tx.Exec("UPDATE messages SET folder_id = ?, uid = ? WHERE id = ?",
				destFolderID, prov, messageID); err != nil {
				return 0, err
			}
			if err := noteFolderChange(tx, messageID, oldDest, destFolderID); err != nil {
				return 0, err
			}
			if err := adjustFolderTotalsTx(tx, oldDest, -1); err != nil {
				return 0, err
			}
			if err := adjustFolderTotalsTx(tx, destFolderID, 1); err != nil {
				return 0, err
			}
			if err := refreshUnreadTx(tx, oldDest); err != nil {
				return 0, err
			}
			if err := refreshUnreadTx(tx, destFolderID); err != nil {
				return 0, err
			}
			if err := insertKeyTombstone(tx, clientKey, tail.ID, accountID, OpMove, messageID, destFolderID, string(raw)); err != nil {
				return 0, err
			}
			return tail.ID, nil
		}
	}
	raw, _ := json.Marshal(MovePayload{ToFolderID: destFolderID, ToFolder: destName})
	validity, verr := folderValidityTx(tx, folderID)
	if verr != nil {
		return 0, verr
	}
	res, err := tx.Exec(`INSERT INTO ops(account_id, type, message_id, payload, prior_folder_id, prior_uid,
		to_folder_id, prior_uidvalidity, identity, status, client_key, created_at)
		VALUES(?, 'move', ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		accountID, messageID, string(raw), folderID, uid, destFolderID, validity, identity, clientKey, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	opID, _ := res.LastInsertId()
	if err := insertKeyTombstone(tx, clientKey, opID, accountID, OpMove, messageID, destFolderID, string(raw)); err != nil {
		return 0, err
	}
	prov := provisionalUIDBase + opID
	if _, err := tx.Exec("UPDATE messages SET folder_id = ?, uid = ? WHERE id = ?",
		destFolderID, prov, messageID); err != nil {
		return 0, err
	}
	if err := noteFolderChange(tx, messageID, folderID, destFolderID); err != nil {
		return 0, err
	}
	if err := adjustFolderTotalsTx(tx, folderID, -1); err != nil {
		return 0, err
	}
	if err := adjustFolderTotalsTx(tx, destFolderID, 1); err != nil {
		return 0, err
	}
	if err := refreshUnreadTx(tx, folderID); err != nil {
		return 0, err
	}
	if err := refreshUnreadTx(tx, destFolderID); err != nil {
		return 0, err
	}
	return opID, nil
}

func (o *Op) PayloadFolderID() int64 {
	var p MovePayload
	if err := json.Unmarshal([]byte(o.Payload), &p); err == nil {
		return p.ToFolderID
	}
	return o.ToFolderID
}

func adjustFolderTotalsTx(tx *sql.Tx, folderID int64, delta int) error {
	_, err := tx.Exec("UPDATE folders SET total = MAX(total + ?, 0) WHERE id = ?", delta, folderID)
	return err
}

func (s *Store) EnqueueExpunge(accountID, messageID int64, clientKey string) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var acctID, folderID int64
	var uid uint32
	if err := tx.QueryRow("SELECT account_id, folder_id, uid FROM messages WHERE id = ?", messageID).
		Scan(&acctID, &folderID, &uid); err != nil {
		return 0, err
	}
	if acctID != accountID {
		return 0, sql.ErrNoRows
	}
	identity, ierr := identityForRowTx(tx, accountID, messageID)
	if ierr != nil {
		return 0, ierr
	}
	if clientKey != "" {
		if prev, err := s.keyPrior(tx, clientKey); err != nil {
			return 0, err
		} else if prev != nil {
			if err := keyReuseErr(prev, accountID, OpExpunge, messageID, identity); err != nil {
				return 0, err
			}
			if err := tx.Commit(); err != nil {
				return 0, err
			}
			return prev.ID, nil
		}
	}
	active, err := s.activeOpsForMessage(tx, messageID)
	if err != nil {
		return 0, err
	}
	for _, o := range active {
		if o.Type == OpExpunge {
			if err := insertKeyTombstone(tx, clientKey, o.ID, accountID, OpExpunge, messageID, 0, "{}"); err != nil {
				return 0, err
			}
			return o.ID, tx.Commit()
		}
	}
	idActive, err := s.activeOpsForIdentity(tx, accountID, identity)
	if err != nil {
		return 0, err
	}
	for _, o := range idActive {
		if o.Type == OpExpunge && o.PriorFolderID == folderID {
			if err := insertKeyTombstone(tx, clientKey, o.ID, accountID, OpExpunge, messageID, 0, "{}"); err != nil {
				return 0, err
			}
			return o.ID, tx.Commit()
		}
	}
	validity, verr := folderValidityTx(tx, folderID)
	if verr != nil {
		return 0, verr
	}
	res, err := tx.Exec(`INSERT INTO ops(account_id, type, message_id, payload, prior_folder_id, prior_uid,
		to_folder_id, prior_uidvalidity, identity, status, client_key, created_at)
		VALUES(?, 'expunge', ?, '{}', ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		accountID, messageID, folderID, uid, folderID, validity, identity, clientKey, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	opID, _ := res.LastInsertId()
	if err := insertKeyTombstone(tx, clientKey, opID, accountID, OpExpunge, messageID, 0, "{}"); err != nil {
		return 0, err
	}
	if err := adjustFolderTotalsTx(tx, folderID, -1); err != nil {
		return 0, err
	}
	if err := refreshUnreadTx(tx, folderID); err != nil {
		return 0, err
	}
	return opID, tx.Commit()
}

func (s *Store) EnqueueSend(accountID int64, payload string, notBefore int64, clientKey string) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if clientKey != "" {
		prev, err := s.keyPrior(tx, clientKey)
		if err != nil {
			return 0, err
		}
		if prev != nil {
			if err := keyReuseErr(prev, accountID, OpSend, 0, ""); err != nil {
				return 0, err
			}
			if prev.Payload != payload {
				return 0, ErrClientKeyConflict
			}
			if err := tx.Commit(); err != nil {
				return 0, err
			}
			return prev.ID, nil
		}
	}
	res, err := tx.Exec(`INSERT INTO ops(account_id, type, message_id, payload, not_before, status, client_key, created_at)
		VALUES(?, 'send', 0, ?, ?, 'pending', ?, ?)`,
		accountID, payload, notBefore, clientKey, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if err := insertKeyTombstone(tx, clientKey, id, accountID, OpSend, 0, 0, payload); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) PriorSendDraftIdentity(accountID, draftID int64) (string, error) {
	rows, err := s.DB.Query(`SELECT client_key, payload FROM ops
		WHERE account_id = ? AND type = 'send' AND client_key LIKE 'sched:%' ORDER BY id DESC`, accountID)
	if err != nil {
		return "", err
	}
	type prior struct {
		key     string
		payload string
	}
	var priors []prior
	for rows.Next() {
		var p prior
		if err := rows.Scan(&p.key, &p.payload); err != nil {
			rows.Close()
			return "", err
		}
		priors = append(priors, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	for _, p := range priors {
		var sp struct {
			Req struct {
				DraftID int64 `json:"draft_id"`
			} `json:"req"`
			DraftIdentity string `json:"draft_identity"`
		}
		if json.Unmarshal([]byte(p.payload), &sp) != nil || sp.Req.DraftID != draftID {
			continue
		}
		if sp.DraftIdentity != "" {
			return sp.DraftIdentity, nil
		}
		return strings.TrimPrefix(p.key, "sched:"), nil
	}
	return "", nil
}

func (s *Store) ClaimDueOps(accountID, now int64, limit int, types ...string) ([]*Op, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	typeFilter := ""
	args := []any{accountID, now, now}
	if len(types) > 0 {
		ph := make([]string, len(types))
		for i, t := range types {
			ph[i] = "?"
			args = append(args, t)
		}
		typeFilter = " AND ops.type IN (" + joinComma(ph) + ")"
	}
	args = append(args, limit)
	rows, err := tx.Query(`SELECT `+opColumns+` FROM ops WHERE account_id = ? AND status = 'pending'
		AND not_before <= ? AND next_attempt <= ?`+typeFilter+`
		AND NOT EXISTS (SELECT 1 FROM ops o2 WHERE o2.id < ops.id
			AND o2.status IN ('pending', 'in_flight', 'needs_attention')
			AND ((o2.message_id = ops.message_id AND ops.message_id <> 0)
				OR (o2.identity <> '' AND o2.identity = ops.identity AND o2.account_id = ops.account_id)))
		ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var out []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, o := range out {
		if _, err := tx.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", o.ID); err != nil {
			return nil, err
		}
		o.Status = OpInFlight
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) AccountsWithDueOps(now int64) ([]int64, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT account_id FROM ops WHERE status = 'pending'
		AND not_before <= ? AND next_attempt <= ?`, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) ReleaseOps(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	_, err := s.DB.Exec("UPDATE ops SET status = 'pending' WHERE status = 'in_flight' AND id IN ("+joinComma(ph)+")", args...)
	return err
}

func (s *Store) ReleaseInFlightOps(accountID int64) (int64, error) {
	res, err := s.DB.Exec("UPDATE ops SET status = 'pending', claim_token = 0 WHERE account_id = ? AND status = 'in_flight' AND sealed = 0", accountID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) StampClaimToken(ids []int64, token int64) error {
	if len(ids) == 0 || token == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, token)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	_, err := s.DB.Exec("UPDATE ops SET claim_token = ? WHERE status = 'in_flight' AND id IN ("+joinComma(ph)+")", args...)
	return err
}

func (s *Store) AccountsWithInFlightOps() ([]int64, error) {
	rows, err := s.DB.Query("SELECT DISTINCT account_id FROM ops WHERE status = 'in_flight'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) HasDueOps(accountID, now int64, types ...string) (bool, error) {
	if len(types) == 0 {
		return false, nil
	}
	ph := make([]string, len(types))
	args := []any{accountID, now, now}
	for i, t := range types {
		ph[i] = "?"
		args = append(args, t)
	}
	var n int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE account_id = ? AND status = 'pending' AND not_before <= ? AND next_attempt <= ? AND type IN ("+joinComma(ph)+")", args...).Scan(&n)
	return n > 0, err
}

func (s *Store) CompleteOp(id int64) error {
	_, err := s.DB.Exec("UPDATE ops SET status = 'done', last_error = '', finished_at = unixepoch() WHERE id = ? AND status = 'in_flight'", id)
	return err
}

func (s *Store) CompleteMoveOp(id, destFolderID int64, messageID int64, realUID uint32) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var opStatus string
	if err := tx.QueryRow("SELECT status FROM ops WHERE id = ?", id).Scan(&opStatus); err != nil {
		return err
	}
	if opStatus != OpInFlight {
		return nil
	}
	var twinFiles []string
	var twinID int64
	err = tx.QueryRow("SELECT id FROM messages WHERE account_id = (SELECT account_id FROM ops WHERE id = ?) AND folder_id = ? AND uid = ? AND id != ?",
		id, destFolderID, realUID, messageID).Scan(&twinID)
	if err == nil {
		paths, qerr := tx.Query("SELECT local_path FROM attachments WHERE message_id = ? AND local_path <> ''", twinID)
		if qerr == nil {
			for paths.Next() {
				var p string
				if err := paths.Scan(&p); err == nil {
					twinFiles = append(twinFiles, p)
				}
			}
			paths.Close()
		}
		if _, err := tx.Exec("DELETE FROM messages_fts WHERE rowid = ?", twinID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM messages WHERE id = ?", twinID); err != nil {
			return err
		}
		if err := adjustFolderTotalsTx(tx, destFolderID, -1); err != nil {
			return err
		}
	}
	res, err := tx.Exec("UPDATE messages SET uid = ?, folder_id = ? WHERE id = ?", realUID, destFolderID, messageID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.Exec("UPDATE ops SET status = 'done', last_error = '', finished_at = unixepoch() WHERE id = ? AND status = 'in_flight'", id); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		for _, p := range twinFiles {
			_ = os.Remove(p)
		}
		return nil
	}
	o, err := scanOp(tx.QueryRow("SELECT " + opColumns + " FROM ops WHERE id = ?", id))
	if err != nil {
		return err
	}
	prov := provisionalUIDBase + o.ID
	if _, err := tx.Exec(`UPDATE ops SET prior_uid = ? WHERE message_id = ? AND status = 'pending'
		AND prior_folder_id = ? AND prior_uid = ?`, realUID, messageID, destFolderID, prov); err != nil {
		return err
	}
	var priorSpecial, destSpecial string
	_ = tx.QueryRow("SELECT COALESCE(special_use, '') FROM folders WHERE id = ?", o.PriorFolderID).Scan(&priorSpecial)
	_ = tx.QueryRow("SELECT COALESCE(special_use, '') FROM folders WHERE id = ?", destFolderID).Scan(&destSpecial)
	if priorSpecial == "all" && destSpecial != "all" && destSpecial != "trash" && destSpecial != "junk" {
		var existing int64
		err := tx.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ?",
			o.AccountID, o.PriorFolderID, o.PriorUID).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == sql.ErrNoRows {
			res2, err := tx.Exec(`INSERT INTO messages(account_id, folder_id, uid, message_id, thread_key, subject,
				from_json, to_json, cc_json, date_ts, flags, snippet, has_attach, size, body_state, trash_origin_folder_id)
				SELECT account_id, ?, ?, message_id, thread_key, subject, from_json, to_json, cc_json, date_ts,
				flags, snippet, has_attach, size, 0, trash_origin_folder_id FROM messages WHERE id = ?`,
				o.PriorFolderID, o.PriorUID, messageID)
			if err != nil {
				return err
			}
			newID, _ := res2.LastInsertId()
			if _, err := tx.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
				SELECT ?, subject, senders, recipients, body FROM messages_fts WHERE rowid = ?`, newID, messageID); err != nil {
				return err
			}
			if err := adjustFolderTotalsTx(tx, o.PriorFolderID, 1); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec("UPDATE ops SET status = 'done', last_error = '', finished_at = unixepoch() WHERE id = ? AND status = 'in_flight'", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, p := range twinFiles {
		_ = os.Remove(p)
	}
	return nil
}

func BackoffSeconds(attempts int) int64 { return backoffSeconds(attempts) }

func backoffSeconds(attempts int) int64 {
	d := int64(5)
	for i := 1; i < attempts && d < 300; i++ {
		d *= 2
	}
	if d > 300 {
		d = 300
	}
	return d
}

func (s *Store) AccelerateAccountOps(accountID, now int64, exclude []int64) error {
	q := "UPDATE ops SET next_attempt = ? WHERE account_id = ? AND status = 'pending' AND next_attempt > ?"
	args := []any{now, accountID, now}
	if len(exclude) > 0 {
		ph := make([]string, len(exclude))
		for i, id := range exclude {
			ph[i] = "?"
			args = append(args, id)
		}
		q += " AND id NOT IN (" + joinComma(ph) + ")"
	}
	_, err := s.DB.Exec(q, args...)
	return err
}

func (s *Store) FailOp(id int64, errText string, permanent bool, now int64) (string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	o, err := scanOp(tx.QueryRow("SELECT " + opColumns + " FROM ops WHERE id = ?", id))
	if err != nil {
		return "", err
	}
	if o.Status != OpInFlight {
		return o.Status, nil
	}
	attempts := o.Attempts + 1
	status := OpPending
	var nextAttempt int64
	undoRequested := o.UndoRequested
	if permanent || attempts >= maxOpAttempts {
		status = OpNeedsAttention
		undoRequested = false
	} else {
		nextAttempt = now + backoffSeconds(attempts)
	}
	if _, err := tx.Exec("UPDATE ops SET status = ?, attempts = ?, next_attempt = ?, last_error = ?, undo_requested = ? WHERE id = ?",
		status, attempts, nextAttempt, errText, undoRequested, id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return status, nil
}

func (s *Store) restorePriorTx(tx *sql.Tx, o *Op) error {
	switch o.Type {
	case OpFlags:
		var folderID int64
		if err := tx.QueryRow("SELECT folder_id FROM messages WHERE id = ?", o.MessageID).Scan(&folderID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE messages SET flags = ? WHERE id = ?", o.PriorFlags, o.MessageID); err != nil {
			return err
		}
		return refreshUnreadTx(tx, folderID)
	case OpMove:
		var curFolderID int64
		if err := tx.QueryRow("SELECT folder_id FROM messages WHERE id = ?", o.MessageID).Scan(&curFolderID); err != nil {
			return err
		}
		if curFolderID != o.PriorFolderID {
			if _, err := tx.Exec("UPDATE messages SET folder_id = ?, uid = ? WHERE id = ?",
				o.PriorFolderID, o.PriorUID, o.MessageID); err != nil {
				return err
			}
			if err := noteFolderChange(tx, o.MessageID, curFolderID, o.PriorFolderID); err != nil {
				return err
			}
			if err := adjustFolderTotalsTx(tx, curFolderID, -1); err != nil {
				return err
			}
			if err := adjustFolderTotalsTx(tx, o.PriorFolderID, 1); err != nil {
				return err
			}
			if err := refreshUnreadTx(tx, curFolderID); err != nil {
				return err
			}
			return refreshUnreadTx(tx, o.PriorFolderID)
		}
		if _, err := tx.Exec("UPDATE messages SET uid = ? WHERE id = ?", o.PriorUID, o.MessageID); err != nil {
			return err
		}
		return nil
	case OpExpunge:
		return errors.Join(
			adjustFolderTotalsTx(tx, o.PriorFolderID, 1),
			refreshUnreadTx(tx, o.PriorFolderID),
		)
	}
	return nil
}

func (s *Store) CancelOp(id int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOp(tx.QueryRow("SELECT " + opColumns + " FROM ops WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOpNotFound
	}
	if err != nil {
		return err
	}
	switch o.Status {
	case OpCancelled:
		return tx.Commit()
	case OpPending, OpNeedsAttention:
	default:
		return ErrOpNotActive
	}
	if o.MessageID != 0 {
		active, err := s.activeOpsForMessage(tx, o.MessageID)
		if err != nil {
			return err
		}
		if n := len(active); n > 0 && active[n-1].ID != o.ID {
			return ErrOpSuperseded
		}
	}
	if err := s.restorePriorTx(tx, o); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE ops SET status = 'cancelled', finished_at = unixepoch() WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RetryOp(id int64) error {
	if o, gerr := s.GetOp(id); gerr == nil && o.Type == OpSend && o.Sealed && o.LastError == SendOutcomeUnknownError {
		return ErrOpNotActive
	}
	res, err := s.DB.Exec(`UPDATE ops SET status = 'pending', attempts = 0, next_attempt = 0, last_error = '', undo_requested = 0
		WHERE id = ? AND status = 'needs_attention'`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrOpNotActive
	}
	return nil
}

func (s *Store) ListActiveOps() ([]*Op, error) {
	rows, err := s.DB.Query("SELECT " + opColumns + " FROM ops WHERE status IN ('pending', 'in_flight') ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) ListAttentionOps() ([]*Op, error) {
	rows, err := s.DB.Query("SELECT " + opColumns + " FROM ops WHERE status = 'needs_attention' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) AttentionCount() (int, error) {
	var n int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE status = 'needs_attention'").Scan(&n)
	return n, err
}

func (s *Store) PendingExpungeSet(ids []int64) map[int64]bool {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.DB.Query(`SELECT message_id, account_id, identity FROM ops WHERE type = 'expunge'
		AND status IN ('pending', 'in_flight', 'needs_attention')
		AND (message_id IN (`+joinComma(ph)+`) OR identity <> '')`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	hints := map[int64]bool{}
	type liveExpunge struct {
		accountID int64
		identity  string
	}
	var lives []liveExpunge
	for rows.Next() {
		var mid, acct int64
		var identity string
		if err := rows.Scan(&mid, &acct, &identity); err != nil {
			continue
		}
		hints[mid] = true
		if identity != "" {
			lives = append(lives, liveExpunge{acct, identity})
		}
	}
	if err := rows.Err(); err != nil {
		return out
	}
	for _, id := range ids {
		if hints[id] {
			out[id] = true
		}
	}
	for _, id := range ids {
		if out[id] || len(lives) == 0 {
			continue
		}
		var acct int64
		if err := s.DB.QueryRow("SELECT account_id FROM messages WHERE id = ?", id).Scan(&acct); err != nil {
			continue
		}
		identity, err := identityForRow(s.DB, acct, id)
		if err != nil {
			continue
		}
		for _, l := range lives {
			if l.accountID == acct && l.identity == identity {
				out[id] = true
				break
			}
		}
	}
	return out
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func (s *Store) HasActiveOpOfType(messageID int64, typ string) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM ops WHERE message_id = ? AND type = ?
		AND status IN ('pending', 'in_flight', 'needs_attention')`, messageID, typ).Scan(&n)
	return n > 0, err
}

type UndoResult struct {
	Mode    string
	Inverse *Op
}

func (s *Store) UndoOp(id int64) (*UndoResult, error) {
	o, err := s.GetOp(id)
	if err != nil {
		return nil, err
	}
	if o.Type == OpSend && o.Sealed && o.Status == OpNeedsAttention && o.LastError == SendOutcomeUnknownError {
		return &UndoResult{Mode: "sealed"}, nil
	}
	switch o.Status {
	case OpInFlight:
		res, err := s.DB.Exec("UPDATE ops SET undo_requested = 1 WHERE id = ? AND status = 'in_flight'", id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return s.UndoOp(id)
		}
		return &UndoResult{Mode: "deferred"}, nil
	case OpCancelled:
		return &UndoResult{Mode: "cancelled"}, nil
	case OpPending, OpNeedsAttention:
		if err := s.CancelOp(id); err != nil {
			if errors.Is(err, ErrOpNotActive) {
				return s.UndoOp(id)
			}
			return nil, err
		}
		return &UndoResult{Mode: "cancelled"}, nil
	case OpDone:
		if o.Type == OpSend {
			return &UndoResult{Mode: "already_sent"}, nil
		}
		if o.Type != OpFlags && o.Type != OpMove {
			return nil, ErrOpCannotUndo
		}
		target, ok, rerr := s.ResolveOpTarget(o)
		if rerr != nil {
			return nil, rerr
		}
		if !ok {
			return nil, ErrUndoTargetGone
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		res, err := tx.Exec("UPDATE ops SET undone = 1 WHERE id = ? AND undone = 0 AND status = 'done'", id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &UndoResult{Mode: "inverse"}, nil
		}
		var invID int64
		switch o.Type {
		case OpFlags:
			seen := o.PriorFlags&1 != 0
			flagged := o.PriorFlags&2 != 0
			invID, _, err = s.enqueueFlagsTx(tx, o.AccountID, target.RowID, &seen, &flagged, "")
		case OpMove:
			invID, err = s.enqueueMoveTx(tx, o.AccountID, target.RowID, o.PriorFolderID, "")
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrUndoTargetGone
			}
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if invID == 0 {
			return &UndoResult{Mode: "inverse"}, nil
		}
		inv, err := s.GetOp(invID)
		if err != nil {
			return &UndoResult{Mode: "inverse"}, nil
		}
		return &UndoResult{Mode: "inverse", Inverse: inv}, nil
	}
	return nil, ErrOpNotActive
}

func (s *Store) RecordUndoFailure(id int64, errText string) error {
	_, err := s.DB.Exec("UPDATE ops SET undo_requested = 0, last_error = ? WHERE id = ?", errText, id)
	return err
}

func (s *Store) CancelInFlightSend(id int64) error {
	res, err := s.DB.Exec("UPDATE ops SET status = 'cancelled', finished_at = unixepoch() WHERE id = ? AND type = 'send' AND status = 'in_flight'", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOpNotActive
	}
	return nil
}

func (s *Store) SealSendForDelivery(id, claimToken, newToken int64) (bool, bool, error) {
	res, err := s.DB.Exec(`UPDATE ops SET claim_token = ?, status = 'in_flight', sealed = 1 WHERE id = ? AND type = 'send'
		AND undo_requested = 0
		AND ((status = 'in_flight' AND claim_token = ?) OR status = 'pending')`, newToken, id, claimToken)
	if err != nil {
		return false, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, false, nil
	}
	o, err := s.GetOp(id)
	if err != nil {
		return false, false, err
	}
	return false, o.UndoRequested && o.ClaimToken == claimToken, nil
}

const SendOutcomeUnknownError = "发送结果未知：发送执行中断，无法确认服务商是否已接受这封邮件"

func (s *Store) ListSealedInFlightSends(accountID int64) ([]*Op, error) {
	rows, err := s.DB.Query("SELECT "+opColumns+" FROM ops WHERE account_id = ? AND type = 'send' AND status = 'in_flight' AND sealed = 1 ORDER BY id", accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) AdoptSealedSend(id, expectToken, newToken int64) (bool, error) {
	res, err := s.DB.Exec("UPDATE ops SET claim_token = ? WHERE id = ? AND type = 'send' AND status = 'in_flight' AND sealed = 1 AND claim_token = ?",
		newToken, id, expectToken)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) ParkSealedSendUnknown(id int64) (bool, error) {
	res, err := s.DB.Exec(`UPDATE ops SET status = 'needs_attention', claim_token = 0, undo_requested = 0, last_error = ?
		WHERE id = ? AND type = 'send' AND status = 'in_flight' AND sealed = 1`, SendOutcomeUnknownError, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) SetSendReturnToDraft(id int64) (bool, error) {
	res, err := s.DB.Exec("UPDATE ops SET return_to_draft = 1 WHERE id = ? AND type = 'send' AND status = 'in_flight'", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Store) ClearUndoRequest(id int64) error {
	_, err := s.DB.Exec("UPDATE ops SET undo_requested = 0 WHERE id = ?", id)
	return err
}

func (s *Store) MessageHasActiveOps(messageID int64) (bool, error) {
	var accountID int64
	if err := s.DB.QueryRow("SELECT account_id FROM messages WHERE id = ?", messageID).Scan(&accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var n int
			err := s.DB.QueryRow(`SELECT COUNT(*) FROM ops WHERE message_id = ?
				AND status IN ('pending', 'in_flight', 'needs_attention')`, messageID).Scan(&n)
			return n > 0, err
		}
		return false, err
	}
	identity, err := identityForRow(s.DB, accountID, messageID)
	if err != nil {
		return false, err
	}
	var n int
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM ops WHERE account_id = ?
		AND status IN ('pending', 'in_flight', 'needs_attention')
		AND (message_id = ? OR (identity <> '' AND identity = ?))`, accountID, messageID, identity).Scan(&n)
	return n > 0, err
}

func (s *Store) DeleteRowOnly(messageID int64) error {
	var folderID int64
	if err := s.DB.QueryRow("SELECT folder_id FROM messages WHERE id = ?", messageID).Scan(&folderID); err != nil {
		return nil
	}
	return s.deleteRow(messageID, folderID, false)
}

func (s *Store) DropMessageRow(messageID int64) error {
	var folderID int64
	if err := s.DB.QueryRow("SELECT folder_id FROM messages WHERE id = ?", messageID).Scan(&folderID); err != nil {
		return nil
	}
	return s.deleteRow(messageID, folderID, true)
}

func (s *Store) deleteRow(messageID, folderID int64, fixCounters bool) error {
	if paths, err := s.DB.Query("SELECT local_path FROM attachments WHERE message_id = ? AND local_path <> ''", messageID); err == nil {
		var files []string
		for paths.Next() {
			var p string
			if err := paths.Scan(&p); err == nil {
				files = append(files, p)
			}
		}
		paths.Close()
		for _, p := range files {
			_ = os.Remove(p)
		}
	}
	_ = s.ftsDelete(messageID)
	oldLen := s.bodyLen(messageID)
	s.beginBodyMutation()
	if _, err := s.DB.Exec("DELETE FROM messages WHERE id = ?", messageID); err != nil {
		s.endBodyMutation(0)
		return err
	}
	s.endBodyMutation(-oldLen)
	if fixCounters {
		if _, err := s.DB.Exec("UPDATE folders SET total = MAX(total - 1, 0) WHERE id = ?", folderID); err != nil {
			return err
		}
		return s.RefreshUnread(folderID)
	}
	return nil
}

const (
	opRetentionSecs   = 3600
	sendRetentionSecs = 7 * 86400
	keyRetentionSecs = 7 * 86400
)

func (s *Store) PurgeTerminalOps(now int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, status FROM ops WHERE status IN ('done', 'cancelled')
		AND COALESCE(NULLIF(finished_at, 0), created_at) < ? - CASE WHEN type = 'send' THEN ? ELSE ? END`,
		now, sendRetentionSecs, opRetentionSecs)
	if err != nil {
		return err
	}
	type deadOp struct {
		id     int64
		status string
	}
	var dead []deadOp
	for rows.Next() {
		var d deadOp
		if err := rows.Scan(&d.id, &d.status); err != nil {
			rows.Close()
			return err
		}
		dead = append(dead, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range dead {
		if _, err := tx.Exec("UPDATE op_client_keys SET status = ?, expires_at = ? WHERE op_id = ? AND expires_at = 0",
			d.status, now+keyRetentionSecs, d.id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM ops WHERE id = ?", d.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM op_client_keys WHERE expires_at != 0 AND expires_at <= ?", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MovedTwinUID(accountID, folderID, messageRowID int64) (uint32, bool) {
	var uid int64
	err := s.DB.QueryRow(`SELECT uid FROM messages WHERE account_id = ? AND folder_id = ?
		AND id != ? AND uid < ? AND message_id <> ''
		AND message_id = (SELECT message_id FROM messages WHERE id = ?)`,
		accountID, folderID, messageRowID, provisionalUIDBase, messageRowID).Scan(&uid)
	if err != nil {
		return 0, false
	}
	return uint32(uid), true
}

func (s *Store) UpdateOpPayload(id int64, payload string) (bool, error) {
	res, err := s.DB.Exec("UPDATE ops SET payload = ? WHERE id = ? AND status IN ('pending', 'in_flight')", payload, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) AccelerateSendOp(id int64) error {
	res, err := s.DB.Exec(`UPDATE ops SET not_before = 0, next_attempt = 0,
		status = CASE WHEN status = 'needs_attention' THEN 'pending' ELSE status END,
		attempts = CASE WHEN status = 'needs_attention' THEN 0 ELSE attempts END
		WHERE id = ? AND type = 'send' AND status IN ('pending', 'needs_attention')`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOpNotActive
	}
	return nil
}
