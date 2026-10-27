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
	bodyComputing  chan struct{}
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
CREATE TABLE IF NOT EXISTS ops (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id      INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	type            TEXT NOT NULL,
	message_id      INTEGER NOT NULL DEFAULT 0,
	payload         TEXT NOT NULL DEFAULT '{}',
	prior_folder_id INTEGER NOT NULL DEFAULT 0,
	prior_uid       INTEGER NOT NULL DEFAULT 0,
	prior_flags     INTEGER NOT NULL DEFAULT 0,
	to_folder_id    INTEGER NOT NULL DEFAULT 0,
	not_before      INTEGER NOT NULL DEFAULT 0,
	status          TEXT NOT NULL DEFAULT 'pending',
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt    INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	client_key      TEXT NOT NULL DEFAULT '',
	undone          INTEGER NOT NULL DEFAULT 0,
	undo_requested  INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ops_client_key ON ops(client_key) WHERE client_key <> '';
CREATE INDEX IF NOT EXISTS idx_ops_account_status ON ops(account_id, status, next_attempt, not_before);
CREATE INDEX IF NOT EXISTS idx_ops_message ON ops(message_id, status);
CREATE TABLE IF NOT EXISTS op_client_keys (
	key_hash     TEXT PRIMARY KEY,
	op_id        INTEGER NOT NULL DEFAULT 0,
	account_id   INTEGER NOT NULL DEFAULT 0,
	type         TEXT NOT NULL DEFAULT '',
	message_id   INTEGER NOT NULL DEFAULT 0,
	to_folder_id INTEGER NOT NULL DEFAULT 0,
	payload      TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL DEFAULT 0,
	expires_at   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash  TEXT PRIMARY KEY,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	wrapped_key TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS access_tokens (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	label        TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE,
	wrapped_key  TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL DEFAULT 0,
	last_used_at INTEGER NOT NULL DEFAULT 0
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
CREATE INDEX IF NOT EXISTS idx_messages_acct_msgid ON messages(account_id, message_id);
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
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
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
CREATE TABLE IF NOT EXISTS gmail_uids (
	account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
	folder_name TEXT NOT NULL,
	uid        INTEGER NOT NULL,
	gmail_id   TEXT NOT NULL,
	PRIMARY KEY (account_id, folder_name, gmail_id),
	UNIQUE (account_id, folder_name, uid)
);
CREATE TABLE IF NOT EXISTS gmail_state (
	account_id INTEGER NOT NULL PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
	history_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS translations (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	target     TEXT NOT NULL,
	engine     TEXT NOT NULL,
	subject    TEXT NOT NULL DEFAULT '',
	html       TEXT NOT NULL DEFAULT '',
	text       TEXT NOT NULL DEFAULT '',
	src_lang   TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (message_id, target, engine)
);
CREATE TABLE IF NOT EXISTS avatars (
	email      TEXT PRIMARY KEY,
	kind       TEXT NOT NULL DEFAULT '',
	mime       TEXT NOT NULL DEFAULT '',
	data       BLOB,
	checked_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tg_pushes (
	message_id INTEGER PRIMARY KEY,
	pushed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS first_seen (
	account_id     INTEGER NOT NULL,
	msg_key        TEXT NOT NULL,
	folder_special TEXT NOT NULL DEFAULT '',
	message_id     INTEGER NOT NULL DEFAULT 0,
	seen_at        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (account_id, msg_key)
);
CREATE TABLE IF NOT EXISTS place_log (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id INTEGER NOT NULL,
	folder_id  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_place_log_msg ON place_log(message_id, seq);
CREATE TRIGGER IF NOT EXISTS trg_place_ins AFTER INSERT ON messages BEGIN
	INSERT INTO place_log(message_id, folder_id) VALUES (NEW.id, NEW.folder_id);
END;
CREATE TRIGGER IF NOT EXISTS trg_place_upd AFTER UPDATE OF folder_id ON messages BEGIN
	INSERT INTO place_log(message_id, folder_id) VALUES (NEW.id, NEW.folder_id);
END;
CREATE TRIGGER IF NOT EXISTS trg_place_del AFTER DELETE ON messages BEGIN
	INSERT INTO place_log(message_id, folder_id) VALUES (OLD.id, OLD.folder_id);
END;
`

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-64000)&_pragma=mmap_size(268435456)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS pending_removals`); err != nil {
		return nil, fmt.Errorf("drop pending removals: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM place_log WHERE seq NOT IN (SELECT MAX(seq) FROM place_log GROUP BY message_id)`); err != nil {
		return nil, fmt.Errorf("prune place log: %w", err)
	}
	if _, err := db.Exec(`UPDATE ops SET status = 'pending' WHERE status = 'in_flight'`); err != nil {
		return nil, fmt.Errorf("reset in-flight ops: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key, value) SELECT 'base_url', value FROM settings WHERE key = 'tg_public_base' ON CONFLICT(key) DO NOTHING`); err != nil {
		return nil, fmt.Errorf("migrate base url: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM settings WHERE key = 'tg_public_base'`); err != nil {
		return nil, fmt.Errorf("migrate base url drop old key: %w", err)
	}
	var firstSeenCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM first_seen`).Scan(&firstSeenCount); err != nil {
		return nil, fmt.Errorf("count first seen: %w", err)
	}
	if firstSeenCount == 0 {
		if _, err := db.Exec(`INSERT OR IGNORE INTO first_seen(account_id, msg_key, folder_special, message_id, seen_at)
			SELECT m.account_id, 'mid:' || m.message_id, f.special_use, m.id, m.date_ts
			FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE m.message_id <> '' ORDER BY m.id`); err != nil {
			return nil, fmt.Errorf("backfill first seen: %w", err)
		}
		rows, err := db.Query(`SELECT m.id, m.account_id, m.from_json, m.to_json, m.cc_json, m.subject, m.date_ts, m.size, f.special_use
			FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE m.message_id = '' ORDER BY m.id`)
		if err != nil {
			return nil, fmt.Errorf("backfill first seen mid-less: %w", err)
		}
		defer rows.Close()
		type midless struct {
			id, accountID, dateTS, size int64
			from, to, cc, subject, spec  string
		}
		var pending []midless
		for rows.Next() {
			var r midless
			if err := rows.Scan(&r.id, &r.accountID, &r.from, &r.to, &r.cc, &r.subject, &r.dateTS, &r.size, &r.spec); err != nil {
				return nil, fmt.Errorf("backfill first seen mid-less scan: %w", err)
			}
			pending = append(pending, r)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("backfill first seen mid-less rows: %w", err)
		}
		rows.Close()
		for _, r := range pending {
			key := "fp:" + envelopeFingerprint(r.from, r.to, r.cc, r.subject, r.dateTS, r.size)
			if _, err := db.Exec(`INSERT OR IGNORE INTO first_seen(account_id, msg_key, folder_special, message_id, seen_at)
				VALUES(?, ?, ?, ?, ?)`, r.accountID, key, r.spec, r.id, r.dateTS); err != nil {
				return nil, fmt.Errorf("backfill first seen mid-less insert: %w", err)
			}
		}
	}
	hasFromName, err := columnExists(db, "accounts", "from_name")
	if err != nil {
		return nil, err
	}
	for _, col := range []struct{ table, name, ddl string }{
		{"messages", "modseq", "ALTER TABLE messages ADD COLUMN modseq INTEGER NOT NULL DEFAULT 0"},
		{"messages", "thread_checked", "ALTER TABLE messages ADD COLUMN thread_checked INTEGER NOT NULL DEFAULT 0"},
		{"messages", "trash_origin_folder_id", "ALTER TABLE messages ADD COLUMN trash_origin_folder_id INTEGER NOT NULL DEFAULT 0"},
		{"messages", "pending_delete", "ALTER TABLE messages ADD COLUMN pending_delete INTEGER NOT NULL DEFAULT 0"},
		{"attachments", "content_id", "ALTER TABLE attachments ADD COLUMN content_id TEXT NOT NULL DEFAULT ''"},
		{"attachments", "inline", "ALTER TABLE attachments ADD COLUMN inline INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "wrapped_key", "ALTER TABLE sessions ADD COLUMN wrapped_key TEXT NOT NULL DEFAULT ''"},
		{"accounts", "signature", "ALTER TABLE accounts ADD COLUMN signature TEXT NOT NULL DEFAULT ''"},
		{"accounts", "from_name", "ALTER TABLE accounts ADD COLUMN from_name TEXT NOT NULL DEFAULT ''"},
		{"accounts", "kind", "ALTER TABLE accounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'imap'"},
		{"ops", "undone", "ALTER TABLE ops ADD COLUMN undone INTEGER NOT NULL DEFAULT 0"},
		{"ops", "undo_requested", "ALTER TABLE ops ADD COLUMN undo_requested INTEGER NOT NULL DEFAULT 0"},
		{"ops", "prior_uidvalidity", "ALTER TABLE ops ADD COLUMN prior_uidvalidity INTEGER NOT NULL DEFAULT 0"},
		{"ops", "finished_at", "ALTER TABLE ops ADD COLUMN finished_at INTEGER NOT NULL DEFAULT 0"},
		{"ops", "identity", "ALTER TABLE ops ADD COLUMN identity TEXT NOT NULL DEFAULT ''"},
		{"ops", "claim_token", "ALTER TABLE ops ADD COLUMN claim_token INTEGER NOT NULL DEFAULT 0"},
		{"ops", "sealed", "ALTER TABLE ops ADD COLUMN sealed INTEGER NOT NULL DEFAULT 0"},
		{"ops", "return_to_draft", "ALTER TABLE ops ADD COLUMN return_to_draft INTEGER NOT NULL DEFAULT 0"},
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
