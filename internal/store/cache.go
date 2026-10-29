package store

import (
	"os"
	"path/filepath"
	"strconv"
)

func (s *Store) WarmBodyBytes() {
	_, _ = s.cacheBodyBytes()
}

func (s *Store) cacheBodyBytes() (int64, error) {
	for {
		s.bodyBytesMu.Lock()
		if s.bodyBytesOK {
			n := s.bodyBytes
			s.bodyBytesMu.Unlock()
			return n, nil
		}
		if s.bodyComputing != nil {
			ch := s.bodyComputing
			s.bodyBytesMu.Unlock()
			<-ch
			continue
		}
		ch := make(chan struct{})
		s.bodyComputing = ch
		epoch := s.bodyEpoch
		s.bodyBytesMu.Unlock()
		stable := epoch%2 == 0
		var n int64
		err := s.DB.QueryRow(`SELECT COALESCE(SUM(LENGTH(text_body) + LENGTH(html_body)), 0) FROM bodies`).Scan(&n)
		s.bodyBytesMu.Lock()
		if err == nil && stable && s.bodyEpoch == epoch {
			s.bodyBytes = n
			s.bodyBytesOK = true
		}
		s.bodyComputing = nil
		close(ch)
		s.bodyBytesMu.Unlock()
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}

func (s *Store) bodyLen(messageID int64) int64 {
	var n int64
	if err := s.DB.QueryRow(`SELECT COALESCE(LENGTH(text_body) + LENGTH(html_body), 0) FROM bodies WHERE message_id = ?`, messageID).Scan(&n); err != nil {
		return 0
	}
	return n
}

func (s *Store) beginBodyMutation() {
	s.bodyBytesMu.Lock()
	s.bodyEpoch++
	s.bodyBytesMu.Unlock()
}

func (s *Store) endBodyMutation(delta int64) {
	s.bodyBytesMu.Lock()
	if s.bodyBytesOK {
		s.bodyBytes += delta
	}
	s.bodyEpoch++
	s.bodyBytesMu.Unlock()
}

func (s *Store) invalidateBodyBytes() {
	s.bodyBytesMu.Lock()
	s.bodyBytesOK = false
	s.bodyEpoch++
	s.bodyBytesMu.Unlock()
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

// A row whose recorded file is gone but whose canonical file exists was
// left pointing at a previous data dir; repoint it so downloads stay local.
func (s *Store) repairAttachmentPaths() error {
	rows, err := s.DB.Query(`SELECT id, local_path FROM attachments WHERE local_path <> ''`)
	if err != nil {
		return err
	}
	var fix []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if p == s.AttachmentFilePath(id) {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if _, err := os.Stat(s.AttachmentFilePath(id)); err == nil {
			fix = append(fix, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range fix {
		if _, err := s.DB.Exec(`UPDATE attachments SET local_path = ? WHERE id = ?`, s.AttachmentFilePath(id), id); err != nil {
			return err
		}
	}
	return nil
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
	oldLen := s.bodyLen(messageID)
	s.beginBodyMutation()
	if _, err := s.DB.Exec("DELETE FROM bodies WHERE message_id = ?", messageID); err != nil {
		s.endBodyMutation(0)
		return err
	}
	s.endBodyMutation(-oldLen)
	if _, err := s.DB.Exec("UPDATE messages SET body_state = 0 WHERE id = ?", messageID); err != nil {
		return err
	}
	return s.ftsSetBody(messageID, "")
}

type CacheRoom struct {
	s         *Store
	bodyBytes int64
	attBytes  int64
}

func (s *Store) NewCacheRoom() (*CacheRoom, error) {
	bodyBytes, attBytes, err := s.CacheUsage()
	if err != nil {
		return nil, err
	}
	return &CacheRoom{s: s, bodyBytes: bodyBytes, attBytes: attBytes}, nil
}

func (r *CacheRoom) NoteBodySaved(size int64) {
	r.bodyBytes += size
}

func (r *CacheRoom) NoteAttachmentSaved(size int64) {
	r.attBytes += size
}

func (r *CacheRoom) MakeRoomForBody(size, dateTS, msgID int64) (bool, error) {
	s := r.s
	usage := r.bodyBytes + r.attBytes
	if usage+size <= s.CacheBudgetBytes() {
		return true, nil
	}
	oldBodies, err := s.bodyBytesBefore(dateTS, msgID)
	if err != nil {
		return false, err
	}
	if usage-r.attBytes-oldBodies+size > s.CacheBudgetBytes() {
		return false, nil
	}
	for usage+size > s.CacheBudgetBytes() {
		if id, path, sz, found, err := s.oldestCachedAttachment(0, 0, 0, true); err != nil {
			return false, err
		} else if found {
			freed, err := s.evictAttachment(id, path, sz)
			if err != nil {
				return false, err
			}
			usage -= freed
			r.attBytes -= freed
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
		r.bodyBytes -= sz
	}
	return true, nil
}

func (r *CacheRoom) MakeRoomForAttachment(size, dateTS, msgID, attID int64) (bool, error) {
	s := r.s
	usage := r.bodyBytes + r.attBytes
	if usage+size <= s.CacheBudgetBytes() {
		return true, nil
	}
	lower, err := s.attachmentBytesBefore(dateTS, msgID, attID)
	if err != nil {
		return false, err
	}
	if usage-lower+size > s.CacheBudgetBytes() {
		return false, nil
	}
	for usage+size > s.CacheBudgetBytes() {
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
		r.attBytes -= freed
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
		WHERE m.account_id = ? AND f.special_use <> 'junk' AND m.body_state IN (1, 2)`, accountID).Scan(&st.BodiesCached)
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
