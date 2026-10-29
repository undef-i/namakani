package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"namakani/internal/store"

	"golang.org/x/crypto/argon2"
)

const (
	kdfTime    = 3
	kdfMemory  = 64 * 1024
	kdfThreads = 2
	kdfKeyLen  = 32
	saltLen    = 16
)

const SessionTTL = 400 * 24 * time.Hour

func hashWithSalt(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, kdfTime, kdfMemory, kdfThreads, kdfKeyLen)
}

func SetupMaster(st *store.Store, password string) error {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if err := st.SetSetting("kdf_salt", base64.StdEncoding.EncodeToString(salt)); err != nil {
		return err
	}
	verifier := verifierOf(hashWithSalt(password, salt))
	if err := st.SetSetting("master_verifier", base64.StdEncoding.EncodeToString(verifier)); err != nil {
		return err
	}
	_, err := DeleteAllSessions(st)
	return err
}

func IsSetup(st *store.Store) (bool, error) {
	_, ok, err := st.GetSetting("master_verifier")
	return ok, err
}

func VerifyMaster(st *store.Store, password string) ([]byte, bool, error) {
	saltB64, ok, err := st.GetSetting("kdf_salt")
	if err != nil || !ok {
		return nil, false, err
	}
	verB64, ok, err := st.GetSetting("master_verifier")
	if err != nil || !ok {
		return nil, false, err
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, false, err
	}
	want, err := base64.StdEncoding.DecodeString(verB64)
	if err != nil {
		return nil, false, err
	}
	key := hashWithSalt(password, salt)
	if subtle.ConstantTimeCompare(verifierOf(key), want) != 1 {
		return nil, false, nil
	}
	return key, true, nil
}

func verifierOf(key []byte) []byte {
	sum := sha256.Sum256(key)
	return sum[:]
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TokenHash(token string) string { return tokenHash(token) }

func NewSession(st *store.Store, wrappedKey string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	now := time.Now()
	_, err := st.DB.Exec("INSERT INTO sessions(token_hash, created_at, expires_at, wrapped_key) VALUES(?, ?, ?, ?)",
		tokenHash(token), now.Unix(), now.Add(SessionTTL).Unix(), wrappedKey)
	if err != nil {
		return "", err
	}
	return token, nil
}

func SessionInfo(st *store.Store, token string) (wrapped string, expiresAt int64, exists bool, err error) {
	err = st.DB.QueryRow("SELECT wrapped_key, expires_at FROM sessions WHERE token_hash = ?",
		tokenHash(token)).Scan(&wrapped, &expiresAt)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return wrapped, expiresAt, true, nil
}

func TouchSession(st *store.Store, token string) error {
	now := time.Now()
	ttl := int64(SessionTTL / time.Second)
	_, err := st.DB.Exec(`UPDATE sessions SET expires_at = min(?, created_at + ?)
		WHERE token_hash = ?
		AND (expires_at < ? OR expires_at > created_at + ?)`,
		now.Add(SessionTTL).Unix(), ttl, tokenHash(token),
		now.Add(SessionTTL-24*time.Hour).Unix(), ttl)
	return err
}

func LoadOrCreateServerKey(path string) ([]byte, error) {
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != 32 {
			return nil, fmt.Errorf("server key file has wrong length %d", len(raw))
		}
		return raw, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

type AccessToken struct {
	ID         int64
	Label      string
	CreatedAt  int64
	LastUsedAt int64
}

func NewAccessToken(st *store.Store, label, wrappedKey string) (int64, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return 0, "", err
	}
	token := hex.EncodeToString(raw)
	res, err := st.DB.Exec("INSERT INTO access_tokens(label, token_hash, wrapped_key, created_at) VALUES(?, ?, ?, ?)",
		label, tokenHash(token), wrappedKey, time.Now().Unix())
	if err != nil {
		return 0, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	return id, token, nil
}

func AccessTokenInfo(st *store.Store, token string) (id int64, wrapped string, exists bool, err error) {
	err = st.DB.QueryRow("SELECT id, wrapped_key FROM access_tokens WHERE token_hash = ?",
		tokenHash(token)).Scan(&id, &wrapped)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return id, wrapped, true, nil
}

func TouchAccessToken(st *store.Store, id int64) error {
	now := time.Now()
	_, err := st.DB.Exec("UPDATE access_tokens SET last_used_at = ? WHERE id = ? AND last_used_at < ?",
		now.Unix(), id, now.Add(-30*time.Second).Unix())
	return err
}

func ListAccessTokens(st *store.Store) ([]AccessToken, error) {
	rows, err := st.DB.Query("SELECT id, label, created_at, last_used_at FROM access_tokens ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessToken
	for rows.Next() {
		var tk AccessToken
		if err := rows.Scan(&tk.ID, &tk.Label, &tk.CreatedAt, &tk.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

func DeleteAccessToken(st *store.Store, id int64) (bool, error) {
	res, err := st.DB.Exec("DELETE FROM access_tokens WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func LiveSessionCount(st *store.Store) (int, error) {
	var n int
	err := st.DB.QueryRow("SELECT COUNT(*) FROM sessions WHERE expires_at > ?", time.Now().Unix()).Scan(&n)
	return n, err
}

func DeleteSession(st *store.Store, token string) error {
	_, err := st.DB.Exec("DELETE FROM sessions WHERE token_hash = ?", tokenHash(token))
	return err
}

func DeleteAllSessions(st *store.Store) (int64, error) {
	res, err := st.DB.Exec("DELETE FROM sessions")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

func Decrypt(key []byte, encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func ParsePort(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		p := int(n)
		if p < 1 || p > 65535 {
			return 0, fmt.Errorf("端口超出范围")
		}
		return p, nil
	case string:
		p, err := strconv.Atoi(n)
		if err != nil || p < 1 || p > 65535 {
			return 0, fmt.Errorf("端口格式不对")
		}
		return p, nil
	default:
		return 0, fmt.Errorf("端口格式不对")
	}
}

func ValidTLSMode(s string) bool {
	return s == "tls" || s == "starttls" || s == "none"
}
