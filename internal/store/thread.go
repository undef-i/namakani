package store

import (
	"strings"
)

func cleanMsgID(s string) string {
	return strings.Trim(strings.TrimSpace(s), "<>")
}

func (s *Store) ComputeThreadKey(accountID int64, messageID, inReplyTo string) string {
	self := cleanMsgID(messageID)
	parent := cleanMsgID(inReplyTo)
	if parent != "" {
		var parentKey string
		err := s.DB.QueryRow("SELECT thread_key FROM messages WHERE account_id = ? AND message_id = ? AND thread_key <> '' LIMIT 1",
			accountID, parent).Scan(&parentKey)
		if err == nil && parentKey != "" {
			return parentKey
		}
		if self != "" && self == parent {
			return self
		}
		return parent
	}
	return self
}

func (s *Store) AdoptThreadKey(accountID int64, fromKey, toKey string, exceptID int64) error {
	if fromKey == "" || toKey == "" || fromKey == toKey {
		return nil
	}
	_, err := s.DB.Exec("UPDATE messages SET thread_key = ? WHERE account_id = ? AND thread_key = ? AND id <> ?",
		toKey, accountID, fromKey, exceptID)
	return err
}

func (s *Store) BackfillThreadKeys() error {
	if _, err := s.DB.Exec("UPDATE messages SET thread_key = '' WHERE thread_key LIKE 'subj:%'"); err != nil {
		return err
	}
	rows, err := s.DB.Query("SELECT id, message_id FROM messages WHERE thread_key = ''")
	if err != nil {
		return err
	}
	type pending struct {
		id        int64
		messageID string
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.messageID); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range list {
		key := cleanMsgID(p.messageID)
		if key == "" {
			continue
		}
		if _, err := s.DB.Exec("UPDATE messages SET thread_key = ? WHERE id = ?", key, p.id); err != nil {
			return err
		}
	}
	return nil
}
