package store

import "os"

type AttachmentRow struct {
	ID        int64  `json:"id"`
	PartPath  string `json:"-"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	ContentID string `json:"-"`
	Inline    bool   `json:"-"`
	LocalPath string `json:"-"`
}

type MessageRoute struct {
	AccountID int64
	FolderID  int64
	Folder    string
	UID       uint32
	BodyState int
}

func (s *Store) MessageRoute(messageID int64) (MessageRoute, error) {
	var r MessageRoute
	err := s.DB.QueryRow(`SELECT m.account_id, m.folder_id, f.name, m.uid, m.body_state
		FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.id = ?`, messageID).
		Scan(&r.AccountID, &r.FolderID, &r.Folder, &r.UID, &r.BodyState)
	return r, err
}

func (s *Store) GetBody(messageID int64) (text, html string, ok bool, err error) {
	err = s.DB.QueryRow("SELECT text_body, html_body FROM bodies WHERE message_id = ?", messageID).
		Scan(&text, &html)
	if err != nil {
		return "", "", false, nil
	}
	return text, html, true, nil
}

func (s *Store) SaveBody(messageID int64, text, html, snippet string) error {
	oldLen := s.bodyLen(messageID)
	s.beginBodyMutation()
	if _, err := s.DB.Exec(`INSERT INTO bodies(message_id, text_body, html_body) VALUES(?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET text_body = excluded.text_body, html_body = excluded.html_body`,
		messageID, text, html); err != nil {
		s.endBodyMutation(0)
		return err
	}
	s.endBodyMutation(s.bodyLen(messageID) - oldLen)
	if _, err := s.DB.Exec("UPDATE messages SET body_state = 1, snippet = ? WHERE id = ?", snippet, messageID); err != nil {
		return err
	}
	return s.ftsSetBody(messageID, text)
}

func (s *Store) ReplaceAttachments(messageID int64, atts []AttachmentRow) ([]AttachmentRow, error) {
	keep := map[string]string{}
	if rows, err := s.DB.Query("SELECT part_path, local_path FROM attachments WHERE message_id = ? AND local_path <> ''", messageID); err == nil {
		for rows.Next() {
			var pp, lp string
			if err := rows.Scan(&pp, &lp); err == nil {
				keep[pp] = lp
			}
		}
		rows.Close()
	}
	if _, err := s.DB.Exec("DELETE FROM attachments WHERE message_id = ?", messageID); err != nil {
		return nil, err
	}
	for _, a := range atts {
		inline := 0
		if a.Inline {
			inline = 1
		}
		if _, err := s.DB.Exec(`INSERT INTO attachments(message_id, part_path, filename, mime, size, content_id, inline, local_path)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
			messageID, a.PartPath, a.Filename, a.Mime, a.Size, a.ContentID, inline, keep[a.PartPath]); err != nil {
			return nil, err
		}
	}
	return s.ListAttachments(messageID)
}

func (s *Store) ListAttachments(messageID int64) ([]AttachmentRow, error) {
	rows, err := s.DB.Query(`SELECT id, part_path, filename, mime, size, content_id, inline, local_path
		FROM attachments WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttachmentRow{}
	for rows.Next() {
		var a AttachmentRow
		var inline int
		if err := rows.Scan(&a.ID, &a.PartPath, &a.Filename, &a.Mime, &a.Size, &a.ContentID, &inline, &a.LocalPath); err != nil {
			return nil, err
		}
		a.Inline = inline != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAttachment(messageID, attachmentID int64) (AttachmentRow, error) {
	var a AttachmentRow
	var inline int
	err := s.DB.QueryRow(`SELECT id, part_path, filename, mime, size, content_id, inline, local_path
		FROM attachments WHERE message_id = ? AND id = ?`, messageID, attachmentID).
		Scan(&a.ID, &a.PartPath, &a.Filename, &a.Mime, &a.Size, &a.ContentID, &inline, &a.LocalPath)
	a.Inline = inline != 0
	return a, err
}

func (s *Store) RefreshUnread(folderID int64) error {
	_, err := s.DB.Exec(`UPDATE folders SET unread =
		(SELECT COUNT(*) FROM messages WHERE folder_id = ? AND (flags & 1) = 0) WHERE id = ?`,
		folderID, folderID)
	return err
}

func (s *Store) SetLocalFlags(messageID int64, flags uint32) error {
	var folderID int64
	if err := s.DB.QueryRow("SELECT folder_id FROM messages WHERE id = ?", messageID).Scan(&folderID); err != nil {
		return err
	}
	if _, err := s.DB.Exec("UPDATE messages SET flags = ? WHERE id = ?", flags, messageID); err != nil {
		return err
	}
	return s.RefreshUnread(folderID)
}

func (s *Store) DeleteLocalMessage(messageID int64) error {
	var folderID int64
	if err := s.DB.QueryRow("SELECT folder_id FROM messages WHERE id = ?", messageID).Scan(&folderID); err != nil {
		return err
	}
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
	if _, err := s.DB.Exec("UPDATE folders SET total = MAX(total - 1, 0) WHERE id = ?", folderID); err != nil {
		return err
	}
	return s.RefreshUnread(folderID)
}

func (s *Store) FolderBySpecial(accountID int64, special string) (int64, string, bool, error) {
	var (
		id   int64
		name string
	)
	err := s.DB.QueryRow("SELECT id, name FROM folders WHERE account_id = ? AND special_use = ? LIMIT 1",
		accountID, special).Scan(&id, &name)
	if err != nil {
		return 0, "", false, nil
	}
	return id, name, true, nil
}

func (s *Store) MoveLocalMessage(oldID, destFolderID int64, destUID uint32) error {
	var (
		acctID    int64
		messageID string
		subject   string
		fromJSON  string
		toJSON    string
		ccJSON    string
		threadKey string
		snippet   string
		dateTS    int64
		flags     uint32
		hasAttach int
		size      int64
		bodyState int
	)
	err := s.DB.QueryRow(`SELECT account_id, message_id, subject, from_json, to_json, cc_json,
		thread_key, snippet, date_ts, flags, has_attach, size, body_state
		FROM messages WHERE id = ?`, oldID).
		Scan(&acctID, &messageID, &subject, &fromJSON, &toJSON, &ccJSON,
			&threadKey, &snippet, &dateTS, &flags, &hasAttach, &size, &bodyState)
	if err != nil {
		return err
	}
	row := &MessageRow{
		UID:       destUID,
		MessageID: messageID,
		ThreadKey: threadKey,
		Subject:   subject,
		FromJSON:  fromJSON,
		ToJSON:    toJSON,
		CcJSON:    ccJSON,
		DateTS:    dateTS,
		Flags:     flags,
		HasAttach: hasAttach != 0,
		Size:      size,
	}
	if _, _, err := s.UpsertMessage(acctID, destFolderID, row); err != nil {
		return err
	}
	var newID int64
	if err := s.DB.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ?",
		acctID, destFolderID, destUID).Scan(&newID); err != nil {
		return err
	}
	if bodyState == 1 {
		copiedLen := s.bodyLen(oldID)
		s.beginBodyMutation()
		res, err := s.DB.Exec(`INSERT OR IGNORE INTO bodies(message_id, text_body, html_body)
			SELECT ?, text_body, html_body FROM bodies WHERE message_id = ?`, newID, oldID)
		if err != nil {
			s.endBodyMutation(0)
			return err
		}
		var delta int64
		if n, err := res.RowsAffected(); err == nil && n == 1 {
			delta = copiedLen
		}
		s.endBodyMutation(delta)
		if _, err := s.DB.Exec(`INSERT INTO attachments(message_id, part_path, filename, mime, size, local_path, content_id, inline)
			SELECT ?, part_path, filename, mime, size, local_path, content_id, inline
			FROM attachments WHERE message_id = ?
			AND NOT EXISTS (SELECT 1 FROM attachments WHERE message_id = ?)`, newID, oldID, newID); err != nil {
			return err
		}
		if _, err := s.DB.Exec("UPDATE messages SET body_state = 1, snippet = ?, thread_key = ? WHERE id = ?",
			snippet, threadKey, newID); err != nil {
			return err
		}
	}
	if err := s.ftsCopy(oldID, newID); err != nil {
		return err
	}
	if err := s.DeleteLocalMessage(oldID); err != nil {
		return err
	}
	if _, err := s.DB.Exec("UPDATE folders SET total = total + 1 WHERE id = ?", destFolderID); err != nil {
		return err
	}
	return s.RefreshUnread(destFolderID)
}

func (s *Store) MessageFlags(messageID int64) (uint32, error) {
	var flags uint32
	err := s.DB.QueryRow("SELECT flags FROM messages WHERE id = ?", messageID).Scan(&flags)
	return flags, err
}
