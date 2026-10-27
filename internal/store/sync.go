package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"time"
)

type FolderState struct {
	ID            int64
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64
}

func (s *Store) UpsertFolder(accountID int64, name, specialUse string) (int64, error) {
	_, err := s.DB.Exec(`INSERT INTO folders(account_id, name, special_use) VALUES(?, ?, ?)
		ON CONFLICT(account_id, name) DO UPDATE SET
		special_use = CASE WHEN excluded.special_use <> '' THEN excluded.special_use ELSE folders.special_use END`,
		accountID, name, specialUse)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.DB.QueryRow("SELECT id FROM folders WHERE account_id = ? AND name = ?", accountID, name).Scan(&id)
	return id, err
}

func (s *Store) FolderState(folderID int64) (FolderState, error) {
	var st FolderState
	st.ID = folderID
	err := s.DB.QueryRow("SELECT uidvalidity, uidnext, highest_modseq FROM folders WHERE id = ?", folderID).
		Scan(&st.UIDValidity, &st.UIDNext, &st.HighestModSeq)
	return st, err
}

func (s *Store) UpdateFolderSync(folderID int64, uidValidity, uidNext uint32, modseq uint64, total int) error {
	_, err := s.DB.Exec(`UPDATE folders SET uidvalidity = ?, uidnext = ?, highest_modseq = ?,
		total = ? + (SELECT COUNT(*) FROM ops WHERE type = 'move'
				AND status IN ('pending', 'in_flight', 'needs_attention') AND to_folder_id = ?)
			- (SELECT COUNT(*) FROM ops WHERE type = 'move'
				AND status IN ('pending', 'in_flight', 'needs_attention') AND prior_folder_id = ? AND to_folder_id <> ?)
			- (SELECT COUNT(*) FROM ops WHERE type = 'expunge'
				AND status IN ('pending', 'in_flight', 'needs_attention') AND prior_folder_id = ?),
		last_sync_at = unixepoch() WHERE id = ?`,
		uidValidity, uidNext, modseq, total, folderID, folderID, folderID, folderID, folderID)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE folders SET unread =
		(SELECT COUNT(*) FROM messages m WHERE m.folder_id = ? AND (m.flags & 1) = 0
			AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id AND o.type = 'expunge'
				AND o.status IN ('pending', 'in_flight', 'needs_attention'))) WHERE id = ?`,
		folderID, folderID)
	return err
}

func (s *Store) WipeFolder(folderID int64) error {
	var files []string
	if paths, err := s.DB.Query(`SELECT local_path FROM attachments WHERE local_path <> ''
		AND message_id IN (SELECT id FROM messages WHERE folder_id = ?)`, folderID); err == nil {
		for paths.Next() {
			var p string
			if err := paths.Scan(&p); err == nil {
				files = append(files, p)
			}
		}
		paths.Close()
	}
	s.beginBodyMutation()
	if _, err := s.DB.Exec(`DELETE FROM messages_fts WHERE rowid IN
		(SELECT id FROM messages WHERE folder_id = ?)`, folderID); err != nil {
		s.endBodyMutation(0)
		return err
	}
	if _, err := s.DB.Exec("DELETE FROM messages WHERE folder_id = ?", folderID); err != nil {
		s.endBodyMutation(0)
		return err
	}
	for _, p := range files {
		_ = os.Remove(p)
	}
	s.invalidateBodyBytes()
	_, err := s.DB.Exec(`UPDATE folders SET uidvalidity = 0, uidnext = 0, highest_modseq = 0,
		total = 0, unread = 0 WHERE id = ?`, folderID)
	return err
}

type MessageRow struct {
	UID         uint32
	MessageID   string
	ThreadKey   string
	Subject     string
	FromJSON    string
	ToJSON      string
	CcJSON      string
	DateTS      int64
	Flags       uint32
	HasAttach   bool
	Size        int64
	ModSeq      uint64
	FirstSeenAt int64
}

func (s *Store) UpsertMessage(accountID, folderID int64, m *MessageRow) (int64, bool, error) {
	hasAttach := 0
	if m.HasAttach {
		hasAttach = 1
	}
	var existing int64
	inserted := s.DB.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ?",
		accountID, folderID, m.UID).Scan(&existing) != nil
	_, err := s.DB.Exec(`INSERT INTO messages(account_id, folder_id, uid, message_id, thread_key, subject,
		from_json, to_json, cc_json, date_ts, flags, has_attach, size, modseq)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, folder_id, uid) DO UPDATE SET
		message_id = excluded.message_id,
		thread_key = CASE WHEN excluded.thread_key <> '' THEN excluded.thread_key ELSE messages.thread_key END,
		subject = excluded.subject,
		from_json = excluded.from_json, to_json = excluded.to_json, cc_json = excluded.cc_json,
		date_ts = excluded.date_ts,
		flags = CASE WHEN EXISTS (SELECT 1 FROM ops WHERE ops.message_id = messages.id AND ops.type = 'flags'
			AND ops.status IN ('pending', 'in_flight', 'needs_attention')) THEN messages.flags ELSE excluded.flags END,
		has_attach = excluded.has_attach, size = excluded.size, modseq = excluded.modseq`,
		accountID, folderID, m.UID, m.MessageID, m.ThreadKey, m.Subject,
		m.FromJSON, m.ToJSON, m.CcJSON, m.DateTS, m.Flags, hasAttach, m.Size, m.ModSeq)
	if err != nil {
		return 0, false, err
	}
	var id int64
	if err := s.DB.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ?",
		accountID, folderID, m.UID).Scan(&id); err != nil {
		return 0, false, err
	}
	if inserted {
		if err := s.recordFirstSeen(accountID, folderID, id, m); err != nil {
			return 0, false, err
		}
	}
	if err := s.ftsUpsertEnvelope(id, m.Subject, m.FromJSON, m.ToJSON, m.CcJSON); err != nil {
		return 0, false, err
	}
	return id, inserted, nil
}

func messageIdentityKey(m *MessageRow) string {
	if m.MessageID != "" {
		return "mid:" + m.MessageID
	}
	return "fp:" + envelopeFingerprint(m.FromJSON, m.ToJSON, m.CcJSON, m.Subject, m.DateTS, m.Size)
}

func envelopeFingerprint(fromJSON, toJSON, ccJSON, subject string, dateTS, size int64) string {
	h := sha256.New()
	for _, part := range []string{fromJSON, toJSON, ccJSON, subject,
		strconv.FormatInt(dateTS, 10), strconv.FormatInt(size, 10)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) recordFirstSeen(accountID, folderID, rowID int64, m *MessageRow) error {
	var special string
	if err := s.DB.QueryRow("SELECT special_use FROM folders WHERE id = ?", folderID).Scan(&special); err != nil {
		return err
	}
	seenAt := m.FirstSeenAt
	if seenAt == 0 {
		seenAt = time.Now().Unix()
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO first_seen(account_id, msg_key, folder_special, message_id, seen_at)
		VALUES(?, ?, ?, ?, ?)`, accountID, messageIdentityKey(m), special, rowID, seenAt)
	return err
}

func (s *Store) UpdateMessageFlags(folderID int64, uid uint32, flags uint32, modseq uint64) (bool, error) {
	var id int64
	var old uint32
	var oldModSeq uint64
	if err := s.DB.QueryRow("SELECT id, flags, modseq FROM messages WHERE folder_id = ? AND uid = ?",
		folderID, uid).Scan(&id, &old, &oldModSeq); err != nil {
		return false, nil
	}
	if modseq > 0 && oldModSeq >= modseq {
		return false, nil
	}
	if active, err := s.HasActiveOpForRow(id, OpFlags); err != nil {
		return false, err
	} else if active {
		return false, nil
	}
	newModSeq := oldModSeq
	if modseq > newModSeq {
		newModSeq = modseq
	}
	if _, err := s.DB.Exec("UPDATE messages SET flags = ?, modseq = ? WHERE folder_id = ? AND uid = ?",
		flags, newModSeq, folderID, uid); err != nil {
		return false, err
	}
	return old != flags, nil
}

func (s *Store) LocalID(folderID int64, uid uint32) (int64, bool) {
	var id int64
	if err := s.DB.QueryRow("SELECT id FROM messages WHERE folder_id = ? AND uid = ?",
		folderID, uid).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}

func (s *Store) ActiveOpPriorUIDs(accountID, folderID int64) (map[uint32]bool, error) {
	rows, err := s.DB.Query(`SELECT prior_uid FROM ops WHERE account_id = ? AND prior_folder_id = ?
		AND status IN ('pending', 'in_flight') AND prior_uid > 0`, accountID, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uint32]bool)
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

func (s *Store) FolderUIDs(folderID int64) (map[uint32]bool, error) {
	rows, err := s.DB.Query("SELECT uid FROM messages WHERE folder_id = ?", folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uint32]bool)
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

func (s *Store) DeleteMessage(folderID int64, uid uint32) error {
	var id int64
	if err := s.DB.QueryRow("SELECT id FROM messages WHERE folder_id = ? AND uid = ?", folderID, uid).Scan(&id); err != nil {
		return nil
	}
	if active, aerr := s.MessageHasActiveOps(id); aerr != nil {
		return aerr
	} else if active {
		return nil
	}
	return s.deleteRowCascade(id)
}

func (s *Store) deleteRowCascade(id int64) error {
	oldLen := s.bodyLen(id)
	_ = s.ftsDelete(id)
	if paths, err := s.DB.Query("SELECT local_path FROM attachments WHERE message_id = ? AND local_path <> ''", id); err == nil {
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
	s.beginBodyMutation()
	_, err := s.DB.Exec("DELETE FROM messages WHERE id = ?", id)
	if err == nil {
		s.endBodyMutation(-oldLen)
	} else {
		s.endBodyMutation(0)
	}
	return err
}

type ClaimRow struct {
	ID        int64
	FolderID  int64
	UID       uint32
	MessageID string
	BodyState int
}

func (s *Store) RowForClaim(folderID int64, uid uint32) (ClaimRow, bool, error) {
	var r ClaimRow
	var pending int
	err := s.DB.QueryRow("SELECT id, folder_id, uid, message_id, body_state, pending_delete FROM messages WHERE folder_id = ? AND uid = ?",
		folderID, uid).Scan(&r.ID, &r.FolderID, &r.UID, &r.MessageID, &r.BodyState, &pending)
	if err != nil {
		return r, false, nil
	}
	return r, pending != 0, nil
}

func (s *Store) SetPendingDelete(id int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.DB.Exec("UPDATE messages SET pending_delete = ? WHERE id = ?", v, id)
	return err
}

func (s *Store) PendingUIDs(folderID int64) (map[uint32]bool, error) {
	rows, err := s.DB.Query("SELECT uid FROM messages WHERE folder_id = ? AND pending_delete <> 0", folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uint32]bool)
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

func (s *Store) TwinRow(accountID int64, messageID string, excludeID int64) (ClaimRow, bool, error) {
	var r ClaimRow
	err := s.DB.QueryRow(`SELECT m.id, m.folder_id, m.uid, m.message_id, m.body_state FROM messages m
		JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND m.message_id = ? AND m.id <> ? AND m.pending_delete = 0
		AND f.special_use <> 'all'
		ORDER BY (m.body_state <> 0) DESC, m.id ASC LIMIT 1`,
		accountID, messageID, excludeID).Scan(&r.ID, &r.FolderID, &r.UID, &r.MessageID, &r.BodyState)
	if err != nil {
		return r, false, nil
	}
	return r, true, nil
}

func (s *Store) PendingRows(accountID int64) ([]ClaimRow, error) {
	rows, err := s.DB.Query(`SELECT id, folder_id, uid, message_id, body_state FROM messages
		WHERE account_id = ? AND pending_delete <> 0 AND message_id <> '' ORDER BY id ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClaimRow
	for rows.Next() {
		var r ClaimRow
		if err := rows.Scan(&r.ID, &r.FolderID, &r.UID, &r.MessageID, &r.BodyState); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) TwinInFolder(accountID, folderID int64, messageID string, excludeID int64) (ClaimRow, bool, error) {
	var r ClaimRow
	err := s.DB.QueryRow(`SELECT m.id, m.folder_id, m.uid, m.message_id, m.body_state FROM messages m
		JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND m.folder_id = ? AND m.message_id = ? AND m.id <> ? AND m.pending_delete = 0
		AND f.special_use <> 'all'
		ORDER BY (m.body_state <> 0) DESC, m.id ASC LIMIT 1`,
		accountID, folderID, messageID, excludeID).Scan(&r.ID, &r.FolderID, &r.UID, &r.MessageID, &r.BodyState)
	if err != nil {
		return r, false, nil
	}
	return r, true, nil
}

func (s *Store) ClaimMove(survivorID, loserID, toFolderID int64, toUID uint32) (bool, error) {
	if active, err := s.MessageHasActiveOps(loserID); err != nil {
		return false, err
	} else if active {
		return false, nil
	}
	var fromFolder int64
	if err := s.DB.QueryRow("SELECT folder_id FROM messages WHERE id = ?", survivorID).Scan(&fromFolder); err != nil {
		return false, err
	}
	if err := s.deleteRowCascade(loserID); err != nil {
		return false, err
	}
	if _, err := s.DB.Exec("UPDATE messages SET folder_id = ?, uid = ?, pending_delete = 0 WHERE id = ?",
		toFolderID, toUID, survivorID); err != nil {
		return false, err
	}
	if err := noteFolderChange(s.DB, survivorID, fromFolder, toFolderID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) CountMessages(accountID int64) (int, error) {
	var n int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE account_id = ?", accountID).Scan(&n)
	return n, err
}
