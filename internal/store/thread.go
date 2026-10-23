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

type ThreadRepairRow struct {
	ID         int64
	MessageID  string
	FolderName string
	UID        uint32
}

func (s *Store) ThreadRepairRows(accountID int64) ([]ThreadRepairRow, error) {
	rows, err := s.DB.Query(`SELECT m.id, m.message_id, f.name, m.uid
		FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND m.message_id <> ''
		AND (m.thread_key = m.message_id OR m.thread_key = '')
		ORDER BY f.name, m.uid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadRepairRow
	for rows.Next() {
		var r ThreadRepairRow
		if err := rows.Scan(&r.ID, &r.MessageID, &r.FolderName, &r.UID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RecomputeThreadKey(accountID, rowID int64, refs []string, inReplyTo string) error {
	var messageID, curKey string
	if err := s.DB.QueryRow("SELECT message_id, thread_key FROM messages WHERE id = ?", rowID).Scan(&messageID, &curKey); err != nil {
		return err
	}
	self := cleanMsgID(messageID)
	if self == "" {
		return nil
	}
	chain := make([]string, 0, len(refs)+1)
	for _, r := range refs {
		if c := cleanMsgID(r); c != "" {
			chain = append(chain, c)
		}
	}
	irt := cleanMsgID(inReplyTo)
	lookup := func(id string) string {
		var k string
		err := s.DB.QueryRow("SELECT thread_key FROM messages WHERE account_id = ? AND message_id = ? AND thread_key <> '' LIMIT 1",
			accountID, id).Scan(&k)
		if err != nil || k == id {
			return ""
		}
		return k
	}
	key := ""
	for i := len(chain) - 1; i >= 0 && key == ""; i-- {
		if chain[i] != self {
			key = lookup(chain[i])
		}
	}
	if key == "" && irt != "" && irt != self {
		key = lookup(irt)
	}
	if key == "" {
		switch {
		case len(chain) > 0:
			key = chain[0]
		case irt != "":
			key = irt
		default:
			key = self
		}
	}
	if _, err := s.DB.Exec("UPDATE messages SET thread_key = ? WHERE id = ?", key, rowID); err != nil {
		return err
	}
	cands := map[string]bool{self: true}
	for _, c := range chain {
		cands[c] = true
	}
	if irt != "" {
		cands[irt] = true
	}
	if curKey != "" {
		cands[curKey] = true
	}
	for c := range cands {
		if c != key {
			if _, err := s.DB.Exec("UPDATE messages SET thread_key = ? WHERE account_id = ? AND thread_key = ?",
				key, accountID, c); err != nil {
				return err
			}
		}
	}
	return nil
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
