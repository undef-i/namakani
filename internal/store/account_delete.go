package store

import (
	"os"
)

func (s *Store) DeleteAccount(accountID int64) error {
	rows, err := s.DB.Query(`SELECT a.id, a.local_path FROM attachments a
		JOIN messages m ON m.id = a.message_id WHERE m.account_id = ?`, accountID)
	if err != nil {
		return err
	}
	type attFile struct {
		id        int64
		localPath string
	}
	var files []attFile
	for rows.Next() {
		var f attFile
		if err := rows.Scan(&f.id, &f.localPath); err != nil {
			rows.Close()
			return err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	s.beginBodyMutation()
	if _, err := s.DB.Exec(
		"DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE account_id = ?)",
		accountID,
	); err != nil {
		s.endBodyMutation(0)
		return err
	}
	if _, err := s.DB.Exec("DELETE FROM op_client_keys WHERE account_id = ?", accountID); err != nil {
		s.endBodyMutation(0)
		return err
	}
	if _, err := s.DB.Exec("DELETE FROM accounts WHERE id = ?", accountID); err != nil {
		s.endBodyMutation(0)
		return err
	}
	s.invalidateBodyBytes()
	for _, f := range files {
		if f.localPath != "" {
			_ = os.Remove(f.localPath)
		}
		_ = os.Remove(s.AttachmentFilePath(f.id))
	}
	_, err = s.DB.Exec("VACUUM")
	return err
}
