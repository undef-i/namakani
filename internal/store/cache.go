package store

import (
	"os"
	"path/filepath"
	"strconv"
)

const CacheBudgetBytes = 4 * 1024 * 1024 * 1024

func (s *Store) cacheBodyBytes() (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COALESCE(SUM(LENGTH(text_body) + LENGTH(html_body)), 0) FROM bodies`).Scan(&n)
	return n, err
}

func (s *Store) cacheAttachmentBytes() (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM attachments WHERE local_path <> ''`).Scan(&n)
	return n, err
}

func (s *Store) CacheUsage() (bodyBytes, attachmentBytes int64, err error) {
	bodyBytes, err = s.cacheBodyBytes()
	if err != nil {
		return 0, 0, err
	}
	attachmentBytes, err = s.cacheAttachmentBytes()
	return bodyBytes, attachmentBytes, err
}

type BodyCandidate struct {
	MessageID  int64
	FolderID   int64
	FolderName string
	UID        uint32
	DateTS     int64
}

func (s *Store) BodyCandidates(accountID int64, limit int) ([]BodyCandidate, error) {
	rows, err := s.DB.Query(`SELECT m.id, m.folder_id, f.name, m.uid, m.date_ts
		FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND m.body_state = 0 AND f.special_use <> 'junk'
		ORDER BY m.date_ts DESC, m.id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BodyCandidate{}
	for rows.Next() {
		var c BodyCandidate
		if err := rows.Scan(&c.MessageID, &c.FolderID, &c.FolderName, &c.UID, &c.DateTS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type AttachmentCandidate struct {
	AttachmentID int64
	MessageID    int64
	FolderID     int64
	FolderName   string
	UID          uint32
	PartPath     string
	Size         int64
	DateTS       int64
}

func (s *Store) AttachmentCandidates(accountID int64, limit int) ([]AttachmentCandidate, error) {
	rows, err := s.DB.Query(`SELECT a.id, a.message_id, m.folder_id, f.name, m.uid, a.part_path, a.size, m.date_ts
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND a.local_path = '' AND f.special_use <> 'junk'
		ORDER BY m.date_ts DESC, m.id DESC, a.id ASC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttachmentCandidate{}
	for rows.Next() {
		var c AttachmentCandidate
		if err := rows.Scan(&c.AttachmentID, &c.MessageID, &c.FolderID, &c.FolderName,
			&c.UID, &c.PartPath, &c.Size, &c.DateTS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetAttachmentLocalPath(attachmentID int64, path string) error {
	_, err := s.DB.Exec("UPDATE attachments SET local_path = ? WHERE id = ?", path, attachmentID)
	return err
}

func (s *Store) AttachmentFilePath(attachmentID int64) string {
	return filepath.Join(s.attachmentsDir, strconv.FormatInt(attachmentID, 10)+".bin")
}

func (s *Store) oldestCachedAttachment(beforeDateTS, beforeMsgID, beforeAttID int64, matchAny bool) (int64, string, int64, bool, error) {
	q := `SELECT a.id, a.local_path, a.size FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE a.local_path <> ''`
	args := []any{}
	if !matchAny {
		q += ` AND (m.date_ts < ? OR (m.date_ts = ? AND m.id < ?) OR (m.date_ts = ? AND m.id = ? AND a.id < ?))`
		args = append(args, beforeDateTS, beforeDateTS, beforeMsgID, beforeDateTS, beforeMsgID, beforeAttID)
	}
	q += ` ORDER BY m.date_ts ASC, m.id ASC, a.id ASC LIMIT 1`
	var (
		id   int64
		path string
		size int64
	)
	err := s.DB.QueryRow(q, args...).Scan(&id, &path, &size)
	if err != nil {
		return 0, "", 0, false, nil
	}
	return id, path, size, true, nil
}

func (s *Store) evictAttachment(id int64, path string, size int64) (int64, error) {
	if path != "" {
		_ = os.Remove(path)
	}
	if _, err := s.DB.Exec("UPDATE attachments SET local_path = '' WHERE id = ?", id); err != nil {
		return 0, err
	}
	return size, nil
}

func (s *Store) oldestCachedBodyBefore(dateTS, msgID int64) (int64, int64, bool, error) {
	var (
		id   int64
		size int64
	)
	err := s.DB.QueryRow(`SELECT b.message_id, LENGTH(b.text_body) + LENGTH(b.html_body)
		FROM bodies b JOIN messages m ON m.id = b.message_id
		WHERE m.date_ts < ? OR (m.date_ts = ? AND m.id < ?)
		ORDER BY m.date_ts ASC, m.id ASC LIMIT 1`, dateTS, dateTS, msgID).Scan(&id, &size)
	if err != nil {
		return 0, 0, false, nil
	}
	return id, size, true, nil
}

func (s *Store) bodyBytesBefore(dateTS, msgID int64) (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COALESCE(SUM(LENGTH(b.text_body) + LENGTH(b.html_body)), 0)
		FROM bodies b JOIN messages m ON m.id = b.message_id
		WHERE m.date_ts < ? OR (m.date_ts = ? AND m.id < ?)`, dateTS, dateTS, msgID).Scan(&n)
	return n, err
}

func (s *Store) attachmentBytesBefore(dateTS, msgID, attID int64) (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COALESCE(SUM(a.size), 0)
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE a.local_path <> '' AND
			(m.date_ts < ? OR (m.date_ts = ? AND m.id < ?) OR (m.date_ts = ? AND m.id = ? AND a.id < ?))`,
		dateTS, dateTS, msgID, dateTS, msgID, attID).Scan(&n)
	return n, err
}

func (s *Store) EvictBody(messageID int64) error {
	if _, err := s.DB.Exec("DELETE FROM bodies WHERE message_id = ?", messageID); err != nil {
		return err
	}
	if _, err := s.DB.Exec("UPDATE messages SET body_state = 0 WHERE id = ?", messageID); err != nil {
		return err
	}
	return s.ftsSetBody(messageID, "")
}

func (s *Store) MakeRoomForBody(size, dateTS, msgID int64) (bool, error) {
	bodyBytes, attBytes, err := s.CacheUsage()
	if err != nil {
		return false, err
	}
	usage := bodyBytes + attBytes
	if usage+size <= CacheBudgetBytes {
		return true, nil
	}
	oldBodies, err := s.bodyBytesBefore(dateTS, msgID)
	if err != nil {
		return false, err
	}
	if usage-attBytes-oldBodies+size > CacheBudgetBytes {
		return false, nil
	}
	for usage+size > CacheBudgetBytes {
		if id, path, sz, found, err := s.oldestCachedAttachment(0, 0, 0, true); err != nil {
			return false, err
		} else if found {
			freed, err := s.evictAttachment(id, path, sz)
			if err != nil {
				return false, err
			}
			usage -= freed
			continue
		}
		id, sz, found, err := s.oldestCachedBodyBefore(dateTS, msgID)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		if err := s.EvictBody(id); err != nil {
			return false, err
		}
		usage -= sz
	}
	return true, nil
}

func (s *Store) MakeRoomForAttachment(size, dateTS, msgID, attID int64) (bool, error) {
	bodyBytes, attBytes, err := s.CacheUsage()
	if err != nil {
		return false, err
	}
	usage := bodyBytes + attBytes
	if usage+size <= CacheBudgetBytes {
		return true, nil
	}
	lower, err := s.attachmentBytesBefore(dateTS, msgID, attID)
	if err != nil {
		return false, err
	}
	if usage-lower+size > CacheBudgetBytes {
		return false, nil
	}
	for usage+size > CacheBudgetBytes {
		id, path, sz, found, err := s.oldestCachedAttachment(dateTS, msgID, attID, false)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		freed, err := s.evictAttachment(id, path, sz)
		if err != nil {
			return false, err
		}
		usage -= freed
	}
	return true, nil
}

type AccountCacheStats struct {
	BodiesCached      int
	BodiesTotal       int
	AttachmentsCached int
	AttachmentsTotal  int
}

func (s *Store) AccountCacheStats(accountID int64) (AccountCacheStats, error) {
	var st AccountCacheStats
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.special_use <> 'junk'`, accountID).Scan(&st.BodiesTotal)
	if err != nil {
		return st, err
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.special_use <> 'junk' AND m.body_state = 1`, accountID).Scan(&st.BodiesCached)
	if err != nil {
		return st, err
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM attachments a
		JOIN messages m ON m.id = a.message_id JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.special_use <> 'junk'`, accountID).Scan(&st.AttachmentsTotal)
	if err != nil {
		return st, err
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM attachments a
		JOIN messages m ON m.id = a.message_id JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND f.special_use <> 'junk' AND a.local_path <> ''`, accountID).Scan(&st.AttachmentsCached)
	return st, err
}
