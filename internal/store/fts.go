package store

import "strings"

func (s *Store) ftsReplace(messageID int64, subject, senders, recipients, body string) error {
	if _, err := s.DB.Exec("DELETE FROM messages_fts WHERE rowid = ?", messageID); err != nil {
		return err
	}
	_, err := s.DB.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
		VALUES(?, ?, ?, ?, ?)`, messageID, subject, senders, recipients, body)
	return err
}

func (s *Store) ftsUpsertEnvelope(messageID int64, subject, fromJSON, toJSON, ccJSON string) error {
	var body string
	_ = s.DB.QueryRow("SELECT body FROM messages_fts WHERE rowid = ?", messageID).Scan(&body)
	return s.ftsReplace(messageID, subject, fromJSON, toJSON+" "+ccJSON, body)
}

func (s *Store) ftsSetBody(messageID int64, body string) error {
	res, err := s.DB.Exec("UPDATE messages_fts SET body = ? WHERE rowid = ?", body, messageID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var subject, fromJ, toJ, ccJ string
	if err := s.DB.QueryRow("SELECT subject, from_json, to_json, cc_json FROM messages WHERE id = ?",
		messageID).Scan(&subject, &fromJ, &toJ, &ccJ); err != nil {
		return err
	}
	return s.ftsReplace(messageID, subject, fromJ, toJ+" "+ccJ, body)
}

func (s *Store) BackfillFTSHTMLBodies() (int, error) {
	total := 0
	var after int64
	for {
		rows, err := s.DB.Query(`SELECT b.message_id, b.html_body FROM bodies b
			LEFT JOIN messages_fts f ON f.rowid = b.message_id
			WHERE b.text_body = '' AND b.html_body <> '' AND (f.body IS NULL OR f.body = '')
			AND b.message_id > ? ORDER BY b.message_id LIMIT 200`, after)
		if err != nil {
			return total, err
		}
		type item struct {
			id   int64
			html string
		}
		var batch []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.html); err != nil {
				rows.Close()
				return total, err
			}
			batch = append(batch, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		if len(batch) == 0 {
			return total, nil
		}
		for _, it := range batch {
			if err := s.ftsSetBody(it.id, HTMLToText(it.html)); err != nil {
				return total, err
			}
			after = it.id
			total++
		}
	}
}

func (s *Store) ftsDelete(messageID int64) error {
	_, err := s.DB.Exec("DELETE FROM messages_fts WHERE rowid = ?", messageID)
	return err
}

func (s *Store) ftsCopy(oldID, newID int64) error {
	var subject, senders, recipients, body string
	err := s.DB.QueryRow("SELECT subject, senders, recipients, body FROM messages_fts WHERE rowid = ?", oldID).
		Scan(&subject, &senders, &recipients, &body)
	if err != nil {
		return nil
	}
	return s.ftsReplace(newID, subject, senders, recipients, body)
}

func BuildFTSQuery(q string) string {
	terms := strings.Fields(q)
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(terms, " ")
}
