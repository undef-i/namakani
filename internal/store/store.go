package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB             *sql.DB
	dataDir        string
	attachmentsDir string
	appMu          sync.RWMutex
	app            AppSettings
	bodyBytesMu    sync.Mutex
	bodyBytes      int64
	bodyBytesOK    bool
	bodyEpoch      int64
}

func (s *Store) DataDir() string { return s.dataDir }

func (s *Store) AttachmentsDir() string { return s.attachmentsDir }

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS push_subscriptions (
	endpoint   TEXT PRIMARY KEY,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS scheduled_sends (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	payload    TEXT NOT NULL DEFAULT '',
	send_at    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT 'pending',
	error      TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash  TEXT PRIMARY KEY,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	wrapped_key TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS accounts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	label       TEXT NOT NULL,
	email       TEXT NOT NULL,
	imap_host   TEXT NOT NULL,
	imap_port   INTEGER NOT NULL,
	imap_tls    TEXT NOT NULL,
	smtp_host   TEXT NOT NULL,
	smtp_port   INTEGER NOT NULL,
	smtp_tls    TEXT NOT NULL,
	tls_skip_verify INTEGER NOT NULL DEFAULT 0,
	username    TEXT NOT NULL,
	enc_password TEXT NOT NULL,
	sort        INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS folders (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	name           TEXT NOT NULL,
	special_use    TEXT NOT NULL DEFAULT '',
	uidvalidity    INTEGER NOT NULL DEFAULT 0,
	uidnext        INTEGER NOT NULL DEFAULT 0,
	highest_modseq INTEGER NOT NULL DEFAULT 0,
	total          INTEGER NOT NULL DEFAULT 0,
	unread         INTEGER NOT NULL DEFAULT 0,
	last_sync_at   INTEGER NOT NULL DEFAULT 0,
	UNIQUE(account_id, name)
);
CREATE TABLE IF NOT EXISTS messages (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	folder_id   INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
	uid         INTEGER NOT NULL,
	message_id  TEXT NOT NULL DEFAULT '',
	thread_key  TEXT NOT NULL DEFAULT '',
	subject     TEXT NOT NULL DEFAULT '',
	from_json   TEXT NOT NULL DEFAULT '{}',
	to_json     TEXT NOT NULL DEFAULT '[]',
	cc_json     TEXT NOT NULL DEFAULT '[]',
	date_ts     INTEGER NOT NULL DEFAULT 0,
	flags       INTEGER NOT NULL DEFAULT 0,
	snippet     TEXT NOT NULL DEFAULT '',
	has_attach  INTEGER NOT NULL DEFAULT 0,
	size        INTEGER NOT NULL DEFAULT 0,
	body_state  INTEGER NOT NULL DEFAULT 0,
	UNIQUE(account_id, folder_id, uid)
);
CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date_ts DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_folder ON messages(folder_id, uid);
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(thread_key);
CREATE INDEX IF NOT EXISTS idx_messages_acct_thread ON messages(account_id, thread_key, date_ts DESC, id DESC);
CREATE TABLE IF NOT EXISTS bodies (
	message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
	text_body  TEXT NOT NULL DEFAULT '',
	html_body  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS attachments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	part_path  TEXT NOT NULL,
	filename   TEXT NOT NULL DEFAULT '',
	mime       TEXT NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,
	local_path TEXT NOT NULL DEFAULT ''
);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	subject, senders, recipients, body,
	tokenize='trigram'
);
CREATE TABLE IF NOT EXISTS proton_uids (
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	folder_name TEXT NOT NULL,
	uid        INTEGER NOT NULL,
	proton_id  TEXT NOT NULL,
	PRIMARY KEY (account_id, folder_name, proton_id),
	UNIQUE (account_id, folder_name, uid)
);
CREATE TABLE IF NOT EXISTS tuta_uids (
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	folder_name TEXT NOT NULL,
	uid        INTEGER NOT NULL,
	tuta_id    TEXT NOT NULL,
	PRIMARY KEY (account_id, folder_name, tuta_id),
	UNIQUE (account_id, folder_name, uid)
);
CREATE TABLE IF NOT EXISTS tuta_sessions (
	account_id   INTEGER NOT NULL PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
	user_id      TEXT NOT NULL DEFAULT '',
	access_token TEXT NOT NULL DEFAULT ''
);
`

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	hasFromName, err := columnExists(db, "accounts", "from_name")
	if err != nil {
		return nil, err
	}
	for _, col := range []struct{ table, name, ddl string }{
		{"messages", "modseq", "ALTER TABLE messages ADD COLUMN modseq INTEGER NOT NULL DEFAULT 0"},
		{"messages", "thread_checked", "ALTER TABLE messages ADD COLUMN thread_checked INTEGER NOT NULL DEFAULT 0"},
		{"attachments", "content_id", "ALTER TABLE attachments ADD COLUMN content_id TEXT NOT NULL DEFAULT ''"},
		{"attachments", "inline", "ALTER TABLE attachments ADD COLUMN inline INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "wrapped_key", "ALTER TABLE sessions ADD COLUMN wrapped_key TEXT NOT NULL DEFAULT ''"},
		{"accounts", "signature", "ALTER TABLE accounts ADD COLUMN signature TEXT NOT NULL DEFAULT ''"},
		{"accounts", "from_name", "ALTER TABLE accounts ADD COLUMN from_name TEXT NOT NULL DEFAULT ''"},
		{"accounts", "kind", "ALTER TABLE accounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'imap'"},
	} {
		has, err := columnExists(db, col.table, col.name)
		if err != nil {
			return nil, err
		}
		if !has {
			if _, err := db.Exec(col.ddl); err != nil {
				return nil, fmt.Errorf("migrate %s.%s: %w", col.table, col.name, err)
			}
		}
	}
	if !hasFromName {
		if _, err := db.Exec(`UPDATE accounts SET from_name = label`); err != nil {
			return nil, fmt.Errorf("backfill from_name: %w", err)
		}
	}
	var ftsSQL string
	_ = db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts'").Scan(&ftsSQL)
	if !strings.Contains(ftsSQL, "trigram") {
		if _, err := db.Exec("DROP TABLE IF EXISTS messages_fts"); err != nil {
			return nil, fmt.Errorf("migrate fts drop: %w", err)
		}
		if _, err := db.Exec(`CREATE VIRTUAL TABLE messages_fts USING fts5(
			subject, senders, recipients, body, tokenize='trigram')`); err != nil {
			return nil, fmt.Errorf("migrate fts create: %w", err)
		}
		if _, err := db.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
			SELECT m.id, m.subject, m.from_json, m.to_json || ' ' || m.cc_json, COALESCE(b.text_body, '')
			FROM messages m LEFT JOIN bodies b ON b.message_id = m.id`); err != nil {
			return nil, fmt.Errorf("migrate fts backfill: %w", err)
		}
	}
	st := &Store{DB: db, dataDir: filepath.Dir(path),
		attachmentsDir: filepath.Join(filepath.Dir(path), "attachments")}
	st.loadAppSettings()
	if err := st.BackfillThreadKeys(); err != nil {
		return nil, fmt.Errorf("backfill thread keys: %w", err)
	}
	return st, nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.DB.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec("INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}
