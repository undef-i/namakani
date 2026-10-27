package store

func (s *Store) GmailUIDMap(accountID int64, folderName string) (map[string]uint32, uint32, error) {
	rows, err := s.DB.Query("SELECT gmail_id, uid FROM gmail_uids WHERE account_id = ? AND folder_name = ? ORDER BY uid",
		accountID, folderName)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make(map[string]uint32)
	var max uint32
	for rows.Next() {
		var gid string
		var uid uint32
		if err := rows.Scan(&gid, &uid); err != nil {
			return nil, 0, err
		}
		out[gid] = uid
		if uid > max {
			max = uid
		}
	}
	return out, max, rows.Err()
}

func (s *Store) AssignGmailUID(accountID int64, folderName, gmailID string) (uint32, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing uint32
	err = tx.QueryRow("SELECT uid FROM gmail_uids WHERE account_id = ? AND folder_name = ? AND gmail_id = ?",
		accountID, folderName, gmailID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	var max uint32
	err = tx.QueryRow("SELECT COALESCE(MAX(uid), 0) FROM gmail_uids WHERE account_id = ? AND folder_name = ?",
		accountID, folderName).Scan(&max)
	if err != nil {
		return 0, err
	}
	uid := max + 1
	if _, err := tx.Exec("INSERT INTO gmail_uids(account_id, folder_name, uid, gmail_id) VALUES(?, ?, ?, ?)",
		accountID, folderName, uid, gmailID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uid, nil
}

func (s *Store) GmailIDForUID(accountID int64, folderName string, uid uint32) (string, bool, error) {
	var gid string
	err := s.DB.QueryRow("SELECT gmail_id FROM gmail_uids WHERE account_id = ? AND folder_name = ? AND uid = ?",
		accountID, folderName, uid).Scan(&gid)
	if err != nil {
		return "", false, nil
	}
	return gid, true, nil
}

type GmailFolderRow struct {
	GmailID string
	UID     uint32
	Flags   uint32
	HasRow  bool
}

func (s *Store) GmailFolderSnapshot(accountID int64, folderName string) ([]GmailFolderRow, error) {
	rows, err := s.DB.Query(`SELECT gu.gmail_id, gu.uid, COALESCE(m.flags, 0), m.id IS NOT NULL
		FROM gmail_uids gu
		JOIN folders f ON f.account_id = gu.account_id AND f.name = gu.folder_name
		LEFT JOIN messages m ON m.folder_id = f.id AND m.uid = gu.uid
		WHERE gu.account_id = ? AND gu.folder_name = ? ORDER BY gu.uid`, accountID, folderName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GmailFolderRow
	for rows.Next() {
		var r GmailFolderRow
		if err := rows.Scan(&r.GmailID, &r.UID, &r.Flags, &r.HasRow); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type GmailEnvelope struct {
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
}

func (s *Store) GmailStoredEnvelope(accountID int64, gmailID string) (GmailEnvelope, bool, error) {
	var env GmailEnvelope
	var hasAttach int
	err := s.DB.QueryRow(`SELECT m.message_id, m.thread_key, m.subject, m.from_json, m.to_json, m.cc_json, m.date_ts, m.flags, m.has_attach, m.size
		FROM gmail_uids gu
		JOIN folders f ON f.account_id = gu.account_id AND f.name = gu.folder_name
		JOIN messages m ON m.folder_id = f.id AND m.uid = gu.uid
		WHERE gu.account_id = ? AND gu.gmail_id = ? LIMIT 1`, accountID, gmailID).
		Scan(&env.MessageID, &env.ThreadKey, &env.Subject, &env.FromJSON, &env.ToJSON, &env.CcJSON, &env.DateTS, &env.Flags, &hasAttach, &env.Size)
	if err != nil {
		return env, false, nil
	}
	env.HasAttach = hasAttach != 0
	return env, true, nil
}

func (s *Store) GmailHistoryID(accountID int64) (string, error) {
	var hid string
	err := s.DB.QueryRow("SELECT history_id FROM gmail_state WHERE account_id = ?", accountID).Scan(&hid)
	if err != nil {
		return "", nil
	}
	return hid, nil
}

func (s *Store) SetGmailHistoryID(accountID int64, historyID string) error {
	_, err := s.DB.Exec("INSERT INTO gmail_state(account_id, history_id) VALUES(?, ?) "+
		"ON CONFLICT(account_id) DO UPDATE SET history_id = excluded.history_id",
		accountID, historyID)
	return err
}
