package store

import (
	"strings"
)

func cleanMsgID(s string) string {
	return strings.Trim(strings.TrimSpace(s), "<>")
}

var subjectPrefixes = []string{"re", "fw", "fwd", "aw", "wg", "回复", "答复", "转发", "回覆"}

func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	for {
		matched := false
		for _, p := range subjectPrefixes {
			if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
				rest := strings.TrimSpace(s[len(p):])
				if strings.HasPrefix(rest, "[") {
					if i := strings.Index(rest, "]"); i >= 0 && i > 1 {
						numeric := true
						for _, r := range rest[1:i] {
							if r < '0' || r > '9' {
								numeric = false
								break
							}
						}
						if numeric {
							rest = strings.TrimSpace(rest[i+1:])
						}
					}
				}
				if strings.HasPrefix(rest, ":") {
					s = strings.TrimSpace(rest[1:])
					matched = true
					break
				}
				if strings.HasPrefix(rest, "：") {
					s = strings.TrimSpace(rest[len("："):])
					matched = true
					break
				}
			}
		}
		if !matched {
			break
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func (s *Store) ComputeThreadKey(accountID int64, messageID, inReplyTo, subject string) string {
	self := cleanMsgID(messageID)
	parent := cleanMsgID(inReplyTo)
	if parent != "" {
		var parentKey, parentSubject string
		err := s.DB.QueryRow("SELECT thread_key, subject FROM messages WHERE account_id = ? AND message_id = ? AND thread_key <> '' LIMIT 1",
			accountID, parent).Scan(&parentKey, &parentSubject)
		if err == nil && parentKey != "" {
			if normalizeSubject(parentSubject) == normalizeSubject(subject) {
				return parentKey
			}
			return self
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
		WHERE m.account_id = ? AND m.message_id <> '' AND m.thread_checked = 0
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

func (s *Store) MarkThreadChecked(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := s.DB.Exec("UPDATE messages SET thread_checked = 1 WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	return err
}

func (s *Store) RecomputeThreadKey(accountID, rowID int64, refs []string, inReplyTo string) error {
	var messageID, curKey, subject string
	if err := s.DB.QueryRow("SELECT message_id, thread_key, subject FROM messages WHERE id = ?", rowID).Scan(&messageID, &curKey, &subject); err != nil {
		return err
	}
	selfSubj := normalizeSubject(subject)
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
	lookup := func(id string) (string, bool) {
		var k, subj string
		err := s.DB.QueryRow("SELECT thread_key, subject FROM messages WHERE account_id = ? AND message_id = ? AND thread_key <> '' LIMIT 1",
			accountID, id).Scan(&k, &subj)
		if err != nil {
			return "", false
		}
		if normalizeSubject(subj) != selfSubj {
			return "", true
		}
		return k, true
	}
	key := ""
	foundAny := false
	for i := len(chain) - 1; i >= 0 && key == ""; i-- {
		if chain[i] != self {
			k, found := lookup(chain[i])
			if found {
				foundAny = true
				key = k
			}
		}
	}
	if key == "" && irt != "" && irt != self {
		k, found := lookup(irt)
		if found {
			foundAny = true
			key = k
		}
	}
	if key == "" {
		switch {
		case foundAny:
			key = self
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
		if c == key || c == "" {
			continue
		}
		rows, err := s.DB.Query("SELECT id, subject FROM messages WHERE account_id = ? AND thread_key = ?", accountID, c)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var rid int64
			var rsubj string
			if err := rows.Scan(&rid, &rsubj); err != nil {
				rows.Close()
				return err
			}
			if rid != rowID && normalizeSubject(rsubj) == selfSubj {
				ids = append(ids, rid)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, rid := range ids {
			if _, err := s.DB.Exec("UPDATE messages SET thread_key = ? WHERE id = ?", key, rid); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) AdoptThreadKey(accountID int64, fromKey, toKey string, exceptID int64, subject string) error {
	if fromKey == "" || toKey == "" || fromKey == toKey {
		return nil
	}
	rows, err := s.DB.Query("SELECT id, subject FROM messages WHERE account_id = ? AND thread_key = ? AND id <> ?",
		accountID, fromKey, exceptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	want := normalizeSubject(subject)
	for rows.Next() {
		var rid int64
		var rsubj string
		if err := rows.Scan(&rid, &rsubj); err != nil {
			return err
		}
		if normalizeSubject(rsubj) == want {
			ids = append(ids, rid)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, rid := range ids {
		if _, err := s.DB.Exec("UPDATE messages SET thread_key = ? WHERE id = ?", toKey, rid); err != nil {
			return err
		}
	}
	return nil
}

