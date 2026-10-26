package store

import (
	"database/sql"
	"errors"
)

// An op's binding to its message is the message's durable identity, never a
// local row id. The row id stored on the op is a cache coordinate refreshed
// by resolution; folder wipes and rebuilds replace rows freely without ever
// touching an op. Identity forms, strongest first:
//
//	p:<provider id>  Proton/Tuta server-side message id
//	m:<message-id>   RFC Message-ID header
//	f:<fingerprint>  envelope fingerprint (same value first_seen uses)
//
// Send ops carry no identity: their object is the payload itself, not a
// pre-existing message.

type idQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

func identityForRow(q idQuerier, accountID, rowID int64) (string, error) {
	var folderID, uid, dateTS, size int64
	var mid, fromJ, toJ, ccJ, subject string
	err := q.QueryRow(`SELECT folder_id, uid, message_id, from_json, to_json, cc_json, subject, date_ts, size
		FROM messages WHERE id = ? AND account_id = ?`, rowID, accountID).
		Scan(&folderID, &uid, &mid, &fromJ, &toJ, &ccJ, &subject, &dateTS, &size)
	if err != nil {
		return "", err
	}
	var kind string
	if err := q.QueryRow("SELECT kind FROM accounts WHERE id = ?", accountID).Scan(&kind); err != nil {
		return "", err
	}
	var folderName string
	if err := q.QueryRow("SELECT name FROM folders WHERE id = ?", folderID).Scan(&folderName); err != nil {
		return "", err
	}
	switch kind {
	case "proton":
		var pid string
		if err := q.QueryRow(`SELECT proton_id FROM proton_uids
			WHERE account_id = ? AND folder_name = ? AND uid = ?`, accountID, folderName, uid).Scan(&pid); err == nil && pid != "" {
			return "p:" + pid, nil
		}
	case "tuta":
		var tid string
		if err := q.QueryRow(`SELECT tuta_id FROM tuta_uids
			WHERE account_id = ? AND folder_name = ? AND uid = ?`, accountID, folderName, uid).Scan(&tid); err == nil && tid != "" {
			return "t:" + tid, nil
		}
	}
	if mid != "" {
		return "m:" + mid, nil
	}
	return "f:" + envelopeFingerprint(fromJ, toJ, ccJ, subject, dateTS, size), nil
}

// IdentityForRow returns the durable identity of one message row.
func (s *Store) IdentityForRow(accountID, rowID int64) (string, error) {
	return identityForRow(s.DB, accountID, rowID)
}

func identityForRowTx(tx *sql.Tx, accountID, rowID int64) (string, error) {
	return identityForRow(tx, accountID, rowID)
}

// ResolvedTarget is one message incarnation resolved from an identity: the
// current row plus its folder/uid route.
type ResolvedTarget struct {
	RowID      int64
	FolderID   int64
	FolderName string
	UID        uint32
}

func targetForRow(q idQuerier, accountID, rowID int64) (ResolvedTarget, error) {
	var t ResolvedTarget
	var uid int64
	err := q.QueryRow(`SELECT m.folder_id, f.name, m.uid FROM messages m
		JOIN folders f ON f.id = m.folder_id WHERE m.id = ? AND m.account_id = ?`, rowID, accountID).
		Scan(&t.FolderID, &t.FolderName, &uid)
	t.RowID = rowID
	t.UID = uint32(uid)
	return t, err
}

func candidateRows(q idQuerier, accountID int64, identity string) ([]int64, error) {
	if len(identity) < 3 || identity[1] != ':' {
		return nil, nil
	}
	val := identity[2:]
	switch identity[0] {
	case 'p', 't':
		table, col := "proton_uids", "proton_id"
		if identity[0] == 't' {
			table, col = "tuta_uids", "tuta_id"
		}
		rows, err := q.Query(`SELECT folder_name, uid FROM `+table+` WHERE account_id = ? AND `+col+` = ?`, accountID, val)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var fname string
			var uid int64
			if err := rows.Scan(&fname, &uid); err != nil {
				return nil, err
			}
			var rowID int64
			err := q.QueryRow(`SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
				WHERE m.account_id = ? AND f.name = ? AND m.uid = ?`, accountID, fname, uid).Scan(&rowID)
			if err == nil {
				out = append(out, rowID)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		return out, rows.Err()
	case 'm':
		rows, err := q.Query("SELECT id FROM messages WHERE account_id = ? AND message_id = ?", accountID, val)
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
	case 'f':
		rows, err := q.Query(`SELECT id, from_json, to_json, cc_json, subject, date_ts, size
			FROM messages WHERE account_id = ? AND message_id = ''`, accountID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id, dateTS, size int64
			var fromJ, toJ, ccJ, subject string
			if err := rows.Scan(&id, &fromJ, &toJ, &ccJ, &subject, &dateTS, &size); err != nil {
				return nil, err
			}
			if envelopeFingerprint(fromJ, toJ, ccJ, subject, dateTS, size) == val {
				out = append(out, id)
			}
		}
		return out, rows.Err()
	}
	return nil, nil
}

// ResolveIdentityRow maps a durable identity to its current incarnation.
// Disambiguation among several live incarnations of one identity (label
// mirrors, cross-folder copies) is deterministic and never a guess: the
// hinted row wins when it sits in the anchor folder, a single incarnation in
// the anchor folder wins next, then the hinted row wherever it now lives,
// then a sole incarnation. Anything else is unresolved: the caller treats
// the target as gone rather than acting on a possibly wrong copy.
func (s *Store) ResolveIdentityRow(accountID int64, identity string, anchorFolderID, hintRowID int64) (ResolvedTarget, bool, error) {
	return resolveIdentityRow(s.DB, accountID, identity, anchorFolderID, hintRowID)
}

func resolveIdentityRow(q idQuerier, accountID int64, identity string, anchorFolderID, hintRowID int64) (ResolvedTarget, bool, error) {
	return resolveIdentityRowMode(q, accountID, identity, anchorFolderID, hintRowID, false)
}

// resolveIdentityRowMode resolves an identity to its current incarnation.
// With moveSource set, a real row counts only inside the anchor folder: a
// move's source is the incarnation the user acted on, and a same-identity
// copy already sitting in the destination is a twin for the completion
// step to claim, never the thing to move.
func resolveIdentityRowMode(q idQuerier, accountID int64, identity string, anchorFolderID, hintRowID int64, moveSource bool) (ResolvedTarget, bool, error) {
	if identity == "" {
		return ResolvedTarget{}, false, nil
	}
	cands, err := candidateRows(q, accountID, identity)
	if err != nil {
		return ResolvedTarget{}, false, err
	}
	if len(cands) == 0 {
		return ResolvedTarget{}, false, nil
	}
	// Real rows are incarnations; provisional rows are optimistic
	// projections written by a live move op and only a last resort. A
	// provisional row's server route is its creator op's prior snapshot,
	// valid only while that route is still free or still holds this
	// identity: a stranger sitting on the route means the message's
	// incarnation is gone, and a same-identity row there is itself the
	// incarnation (the projection is then redundant).
	var reals, provs []ResolvedTarget
	for _, id := range cands {
		t, err := targetForRow(q, accountID, id)
		if err != nil {
			return ResolvedTarget{}, false, err
		}
		if t.UID < provisionalUIDBase {
			reals = append(reals, t)
			continue
		}
		var status, typ string
		var pfid, puid int64
		err = q.QueryRow("SELECT status, type, prior_folder_id, prior_uid FROM ops WHERE id = ?",
			int64(t.UID)-provisionalUIDBase).Scan(&status, &typ, &pfid, &puid)
		if err != nil || typ != OpMove || !isLiveOpStatus(status) || pfid == 0 {
			provs = append(provs, t)
			continue
		}
		var occID int64
		errOcc := q.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ?",
			accountID, pfid, puid).Scan(&occID)
		if errOcc == nil {
			if _, ierr := identityForRow(q, accountID, occID); ierr == nil {
				continue
			}
		}
		var fname string
		if err := q.QueryRow("SELECT name FROM folders WHERE id = ?", pfid).Scan(&fname); err == nil {
			t.FolderID, t.FolderName, t.UID = pfid, fname, uint32(puid)
		}
		provs = append(provs, t)
	}
	pick := func(targets []ResolvedTarget, anchorOnly bool) (ResolvedTarget, bool) {
		inAnchor := []ResolvedTarget{}
		var hinted *ResolvedTarget
		for i := range targets {
			if targets[i].FolderID == anchorFolderID {
				inAnchor = append(inAnchor, targets[i])
			}
			if targets[i].RowID == hintRowID {
				tt := targets[i]
				hinted = &tt
			}
		}
		switch {
		case hinted != nil && hinted.FolderID == anchorFolderID:
			return *hinted, true
		case len(inAnchor) == 1:
			return inAnchor[0], true
		case anchorOnly:
			return ResolvedTarget{}, false
		case hinted != nil:
			return *hinted, true
		case len(targets) == 1:
			return targets[0], true
		}
		return ResolvedTarget{}, false
	}
	if t, ok := pick(reals, moveSource); ok {
		return t, true, nil
	}
	if t, ok := pick(provs, false); ok {
		return t, true, nil
	}
	return ResolvedTarget{}, false, nil
}

func isLiveOpStatus(status string) bool {
	return status == OpPending || status == OpInFlight || status == OpNeedsAttention
}

// ResolveOpTarget resolves an op's identity to the message's current
// incarnation and refreshes the op's cached row coordinate when it moved.
func (s *Store) ResolveOpTarget(o *Op) (ResolvedTarget, bool, error) {
	return s.resolveOpTargetMode(o, false)
}

// ResolveMoveSource resolves a move op's source incarnation: a real row
// inside the folder the user acted from, or the op's own provisional
// projection unwrapped to its server route.
func (s *Store) ResolveMoveSource(o *Op) (ResolvedTarget, bool, error) {
	return s.resolveOpTargetMode(o, true)
}

func (s *Store) resolveOpTargetMode(o *Op, moveSource bool) (ResolvedTarget, bool, error) {
	t, ok, err := resolveIdentityRowMode(s.DB, o.AccountID, o.Identity, o.PriorFolderID, o.MessageID, moveSource)
	if err != nil || !ok {
		return t, ok, err
	}
	if t.RowID != o.MessageID {
		if _, err := s.DB.Exec("UPDATE ops SET message_id = ? WHERE id = ?", t.RowID, o.ID); err != nil {
			return t, false, err
		}
	}
	return t, true, nil
}

// RefreshOpHints re-resolves every live message-bound op of the account,
// refreshing cached row coordinates. It never changes an op's status: only
// execution pronounces a target gone.
func (s *Store) RefreshOpHints(accountID int64) (int, error) {
	rows, err := s.DB.Query(`SELECT `+opColumns+` FROM ops WHERE account_id = ? AND identity <> ''
		AND status IN ('pending', 'in_flight', 'needs_attention')`, accountID)
	if err != nil {
		return 0, err
	}
	var ops []*Op
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		ops = append(ops, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	changed := 0
	for _, o := range ops {
		t, ok, err := s.ResolveOpTarget(o)
		if err != nil {
			return changed, err
		}
		if ok && t.RowID != o.MessageID {
			changed++
		}
	}
	return changed, nil
}

// RecomputeFolderUnread rewrites a folder's unread counter from the current
// rows, honouring live expunge ops through their (refreshed) row coordinates.
func (s *Store) RecomputeFolderUnread(folderID int64) error {
	_, err := s.DB.Exec(`UPDATE folders SET unread =
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = ? AND (m.flags & 1) = 0
			AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id AND o.type = 'expunge'
				AND o.status IN ('pending', 'in_flight', 'needs_attention'))) WHERE id = ?`,
		folderID, folderID)
	return err
}

// HasActiveOpForRow reports whether a live op of the given type binds the
// message this row incarnates, by row coordinate or by identity.
func (s *Store) HasActiveOpForRow(rowID int64, typ string) (bool, error) {
	var accountID int64
	if err := s.DB.QueryRow("SELECT account_id FROM messages WHERE id = ?", rowID).Scan(&accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	identity, err := identityForRow(s.DB, accountID, rowID)
	if err != nil {
		return false, err
	}
	var n int
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM ops WHERE type = ? AND account_id = ?
		AND status IN ('pending', 'in_flight', 'needs_attention')
		AND (message_id = ? OR (identity <> '' AND identity = ?))`,
		typ, accountID, rowID, identity).Scan(&n)
	return n > 0, err
}

