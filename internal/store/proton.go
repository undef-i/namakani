package store

func (s *Store) ProtonUIDMap(accountID int64, folderName string) (map[string]uint32, uint32, error) {
	rows, err := s.DB.Query("SELECT proton_id, uid FROM proton_uids WHERE account_id = ? AND folder_name = ? ORDER BY uid",
		accountID, folderName)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make(map[string]uint32)
	var max uint32
	for rows.Next() {
		var pid string
		var uid uint32
		if err := rows.Scan(&pid, &uid); err != nil {
			return nil, 0, err
		}
		out[pid] = uid
		if uid > max {
			max = uid
		}
	}
	return out, max, rows.Err()
}

func (s *Store) AssignProtonUID(accountID int64, folderName, protonID string) (uint32, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing uint32
	err = tx.QueryRow("SELECT uid FROM proton_uids WHERE account_id = ? AND folder_name = ? AND proton_id = ?",
		accountID, folderName, protonID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	var max uint32
	err = tx.QueryRow("SELECT COALESCE(MAX(uid), 0) FROM proton_uids WHERE account_id = ? AND folder_name = ?",
		accountID, folderName).Scan(&max)
	if err != nil {
		return 0, err
	}
	uid := max + 1
	if _, err := tx.Exec("INSERT INTO proton_uids(account_id, folder_name, uid, proton_id) VALUES(?, ?, ?, ?)",
		accountID, folderName, uid, protonID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uid, nil
}

func (s *Store) ProtonFoldersForID(accountID int64, protonID string) ([]string, error) {
	rows, err := s.DB.Query("SELECT DISTINCT folder_name FROM proton_uids WHERE account_id = ? AND proton_id = ?",
		accountID, protonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) FolderTotals(accountID int64) (map[string]int, error) {
	rows, err := s.DB.Query("SELECT name, total FROM folders WHERE account_id = ?", accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var name string
		var total int
		if err := rows.Scan(&name, &total); err != nil {
			return nil, err
		}
		out[name] = total
	}
	return out, rows.Err()
}

func (s *Store) ProtonIDForUID(accountID int64, folderName string, uid uint32) (string, bool, error) {
	var pid string
	err := s.DB.QueryRow("SELECT proton_id FROM proton_uids WHERE account_id = ? AND folder_name = ? AND uid = ?",
		accountID, folderName, uid).Scan(&pid)
	if err != nil {
		return "", false, nil
	}
	return pid, true, nil
}
