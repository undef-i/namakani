package store

type TutaSession struct {
	UserID      string
	AccessToken string
}

func (s *Store) SaveTutaSession(accountID int64, userID, accessToken string) error {
	_, err := s.DB.Exec("INSERT INTO tuta_sessions(account_id, user_id, access_token) VALUES(?, ?, ?) "+
		"ON CONFLICT(account_id) DO UPDATE SET user_id = excluded.user_id, access_token = excluded.access_token",
		accountID, userID, accessToken)
	return err
}

func (s *Store) LoadTutaSession(accountID int64) (*TutaSession, error) {
	var ts TutaSession
	err := s.DB.QueryRow("SELECT user_id, access_token FROM tuta_sessions WHERE account_id = ?", accountID).
		Scan(&ts.UserID, &ts.AccessToken)
	if err != nil {
		return nil, nil
	}
	return &ts, nil
}

func (s *Store) DeleteTutaSession(accountID int64) error {
	_, err := s.DB.Exec("DELETE FROM tuta_sessions WHERE account_id = ?", accountID)
	return err
}

func (s *Store) TutaUIDMap(accountID int64, folderName string) (map[string]uint32, uint32, error) {
	rows, err := s.DB.Query("SELECT tuta_id, uid FROM tuta_uids WHERE account_id = ? AND folder_name = ? ORDER BY uid",
		accountID, folderName)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make(map[string]uint32)
	var max uint32
	for rows.Next() {
		var tid string
		var uid uint32
		if err := rows.Scan(&tid, &uid); err != nil {
			return nil, 0, err
		}
		out[tid] = uid
		if uid > max {
			max = uid
		}
	}
	return out, max, rows.Err()
}

func (s *Store) AssignTutaUID(accountID int64, folderName, tutaID string) (uint32, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing uint32
	err = tx.QueryRow("SELECT uid FROM tuta_uids WHERE account_id = ? AND folder_name = ? AND tuta_id = ?",
		accountID, folderName, tutaID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	var max uint32
	err = tx.QueryRow("SELECT COALESCE(MAX(uid), 0) FROM tuta_uids WHERE account_id = ? AND folder_name = ?",
		accountID, folderName).Scan(&max)
	if err != nil {
		return 0, err
	}
	uid := max + 1
	if _, err := tx.Exec("INSERT INTO tuta_uids(account_id, folder_name, uid, tuta_id) VALUES(?, ?, ?, ?)",
		accountID, folderName, uid, tutaID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uid, nil
}

func (s *Store) TutaIDForUID(accountID int64, folderName string, uid uint32) (string, bool, error) {
	var tid string
	err := s.DB.QueryRow("SELECT tuta_id FROM tuta_uids WHERE account_id = ? AND folder_name = ? AND uid = ?",
		accountID, folderName, uid).Scan(&tid)
	if err != nil {
		return "", false, nil
	}
	return tid, true, nil
}
