package store

import "database/sql"

type TranslationRow struct {
	Subject string
	HTML    string
	Text    string
	SrcLang string
}

func (s *Store) GetTranslation(messageID int64, target, engine string) (*TranslationRow, bool, error) {
	var row TranslationRow
	err := s.DB.QueryRow(`SELECT subject, html, text, src_lang FROM translations
		WHERE message_id = ? AND target = ? AND engine = ?`, messageID, target, engine).
		Scan(&row.Subject, &row.HTML, &row.Text, &row.SrcLang)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &row, true, nil
}

func (s *Store) PutTranslation(messageID int64, target, engine string, row *TranslationRow) error {
	_, err := s.DB.Exec(`INSERT INTO translations(message_id, target, engine, subject, html, text, src_lang, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, unixepoch())
		ON CONFLICT(message_id, target, engine) DO UPDATE SET
			subject = excluded.subject, html = excluded.html, text = excluded.text,
			src_lang = excluded.src_lang, created_at = excluded.created_at`,
		messageID, target, engine, row.Subject, row.HTML, row.Text, row.SrcLang)
	return err
}

type AvatarRow struct {
	Kind      string
	Mime      string
	Data      []byte
	CheckedAt int64
}

func (s *Store) GetAvatar(email string) (*AvatarRow, bool, error) {
	var row AvatarRow
	err := s.DB.QueryRow(`SELECT kind, mime, data, checked_at FROM avatars WHERE email = ?`, email).
		Scan(&row.Kind, &row.Mime, &row.Data, &row.CheckedAt)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &row, true, nil
}

func (s *Store) PutAvatar(email, kind, mime string, data []byte, checkedAt int64) error {
	_, err := s.DB.Exec(`INSERT INTO avatars(email, kind, mime, data, checked_at) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET kind = excluded.kind, mime = excluded.mime,
			data = excluded.data, checked_at = excluded.checked_at`,
		email, kind, mime, data, checkedAt)
	return err
}
