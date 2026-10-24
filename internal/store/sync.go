package store

import "os"

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
		total = ?, last_sync_at = unixepoch() WHERE id = ?`,
		uidValidity, uidNext, modseq, total, folderID)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE folders SET unread =
		(SELECT COUNT(*) FROM messages WHERE folder_id = ? AND (flags & 1) = 0) WHERE id = ?`,
		folderID, folderID)
	return err
}

func (s *Store) WipeFolder(folderID int64) error {
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
	s.invalidateBodyBytes()
	_, err := s.DB.Exec(`UPDATE folders SET uidvalidity = 0, uidnext = 0, highest_modseq = 0,
		total = 0, unread = 0 WHERE id = ?`, folderID)
	return err
}

type MessageRow struct {
	UID       uint32
	MessageID string
	ThreadKey string
	Subject   string
	FromJSON  string
	ToJSON    string
	CcJSON    string
	DateTS    int64
	Flags     uint32
	HasAttach bool
	Size      int64
	ModSeq    uint64
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
		date_ts = excluded.date_ts, flags = excluded.flags,
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
	if err := s.ftsUpsertEnvelope(id, m.Subject, m.FromJSON, m.ToJSON, m.CcJSON); err != nil {
		return 0, false, err
	}
	return id, inserted, nil
}

func (s *Store) UpdateMessageFlags(folderID int64, uid uint32, flags uint32, modseq uint64) (bool, error) {
	var old uint32
	if err := s.DB.QueryRow("SELECT flags FROM messages WHERE folder_id = ? AND uid = ?",
		folderID, uid).Scan(&old); err != nil {
		return false, nil
	}
	if _, err := s.DB.Exec("UPDATE messages SET flags = ?, modseq = ? WHERE folder_id = ? AND uid = ?",
		flags, modseq, folderID, uid); err != nil {
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
	var oldLen int64
	if err := s.DB.QueryRow("SELECT id FROM messages WHERE folder_id = ? AND uid = ?", folderID, uid).Scan(&id); err == nil {
		oldLen = s.bodyLen(id)
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
	}
	s.beginBodyMutation()
	_, err := s.DB.Exec("DELETE FROM messages WHERE folder_id = ? AND uid = ?", folderID, uid)
	if err == nil {
		s.endBodyMutation(-oldLen)
	} else {
		s.endBodyMutation(0)
	}
	return err
}

func (s *Store) CountMessages(accountID int64) (int, error) {
	var n int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE account_id = ?", accountID).Scan(&n)
	return n, err
}
