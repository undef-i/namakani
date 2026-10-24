package store

import (
	"os"
	"strings"
)

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

func (s *Store) MarkPendingRemoval(messageID, fromFolderID int64) {
	_, _ = s.DB.Exec(`INSERT OR REPLACE INTO pending_removals(message_id, from_folder_id, created_at) VALUES(?, ?, unixepoch())`, messageID, fromFolderID)
}

func (s *Store) ClearPendingRemoval(messageID int64) {
	_, _ = s.DB.Exec(`DELETE FROM pending_removals WHERE message_id = ?`, messageID)
}

func (s *Store) PendingRemovalSet(ids []int64) map[int64]bool {
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
	rows, err := s.DB.Query(`SELECT message_id FROM pending_removals WHERE message_id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out[id] = true
		}
	}
	return out
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
	var acctID, srcFolderID int64
	if err := s.DB.QueryRow("SELECT account_id, folder_id FROM messages WHERE id = ?", oldID).Scan(&acctID, &srcFolderID); err != nil {
		return err
	}
	var dupID int64
	if err := s.DB.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ? AND id != ?",
		acctID, destFolderID, destUID, oldID).Scan(&dupID); err == nil {
		if err := s.DeleteLocalMessage(dupID); err != nil {
			return err
		}
	}
	if _, err := s.DB.Exec("UPDATE messages SET folder_id = ?, uid = ? WHERE id = ?", destFolderID, destUID, oldID); err != nil {
		return err
	}
	if srcFolderID != destFolderID {
		if _, err := s.DB.Exec("UPDATE folders SET total = MAX(total - 1, 0) WHERE id = ?", srcFolderID); err != nil {
			return err
		}
		if _, err := s.DB.Exec("UPDATE folders SET total = total + 1 WHERE id = ?", destFolderID); err != nil {
			return err
		}
		if err := s.RefreshUnread(srcFolderID); err != nil {
			return err
		}
	}
	return s.RefreshUnread(destFolderID)
}

func (s *Store) MessageFlags(messageID int64) (uint32, error) {
	var flags uint32
	err := s.DB.QueryRow("SELECT flags FROM messages WHERE id = ?", messageID).Scan(&flags)
	return flags, err
}
