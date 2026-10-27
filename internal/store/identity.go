package store

import (
	"database/sql"
	"errors"
)

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
	if uid >= provisionalUIDBase {
		var opAcct int64
		var opType, opIdentity string
		if err := q.QueryRow("SELECT account_id, type, identity FROM ops WHERE id = ?",
			uid-provisionalUIDBase).Scan(&opAcct, &opType, &opIdentity); err == nil &&
			opAcct == accountID && opType == OpMove && opIdentity != "" {
			return opIdentity, nil
		}
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
	case "gmail":
		var gid string
		if err := q.QueryRow(`SELECT gmail_id FROM gmail_uids
			WHERE account_id = ? AND folder_name = ? AND uid = ?`, accountID, folderName, uid).Scan(&gid); err == nil && gid != "" {
			return "g:" + gid, nil
		}
	}
	if mid != "" {
		return "m:" + mid, nil
	}
	return "f:" + envelopeFingerprint(fromJ, toJ, ccJ, subject, dateTS, size), nil
}

func (s *Store) IdentityForRow(accountID, rowID int64) (string, error) {
	return identityForRow(s.DB, accountID, rowID)
}

func identityForRowTx(tx *sql.Tx, accountID, rowID int64) (string, error) {
	return identityForRow(tx, accountID, rowID)
}

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
	case 'p', 't', 'g':
		table, col := "proton_uids", "proton_id"
		if identity[0] == 't' {
			table, col = "tuta_uids", "tuta_id"
		}
		if identity[0] == 'g' {
			table, col = "gmail_uids", "gmail_id"
		}
		rows, err := q.Query(`SELECT folder_name, uid FROM `+table+` WHERE account_id = ? AND `+col+` = ?`, accountID, val)
		if err != nil {
			return nil, err
		}
		type mapping struct {
			fname string
			uid   int64
		}
		var maps []mapping
		for rows.Next() {
			var mp mapping
			if err := rows.Scan(&mp.fname, &mp.uid); err != nil {
				rows.Close()
				return nil, err
			}
			maps = append(maps, mp)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var out []int64
		for _, mp := range maps {
			var rowID int64
			err := q.QueryRow(`SELECT m.id FROM messages m JOIN folders f ON f.id = m.folder_id
				WHERE m.account_id = ? AND f.name = ? AND m.uid = ?`, accountID, mp.fname, mp.uid).Scan(&rowID)
			if err == nil {
				out = append(out, rowID)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		provRows, err := q.Query(`SELECT m.id FROM messages m JOIN ops o
			ON o.id = m.uid - ? AND o.type = ? AND o.account_id = m.account_id
			WHERE m.account_id = ? AND m.uid >= ? AND o.identity = ?`,
			provisionalUIDBase, OpMove, accountID, provisionalUIDBase, identity)
		if err != nil {
			return nil, err
		}
		defer provRows.Close()
		for provRows.Next() {
			var id int64
			if err := provRows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, provRows.Err()
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

func (s *Store) ResolveIdentityRow(accountID int64, identity string, anchorFolderID, hintRowID int64) (ResolvedTarget, bool, error) {
	return resolveIdentityRow(s.DB, accountID, identity, anchorFolderID, hintRowID)
}

func resolveIdentityRow(q idQuerier, accountID int64, identity string, anchorFolderID, hintRowID int64) (ResolvedTarget, bool, error) {
	return resolveIdentityRowMode(q, accountID, identity, anchorFolderID, hintRowID, false)
}

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

func (s *Store) ResolveOpTarget(o *Op) (ResolvedTarget, bool, error) {
	return s.resolveOpTargetMode(o, false)
}

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

func (s *Store) RecomputeFolderUnread(folderID int64) error {
	_, err := s.DB.Exec(`UPDATE folders SET unread =
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = ? AND (m.flags & 1) = 0
			AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id AND o.type = 'expunge'
				AND o.status IN ('pending', 'in_flight', 'needs_attention'))) WHERE id = ?`,
		folderID, folderID)
	return err
}

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

