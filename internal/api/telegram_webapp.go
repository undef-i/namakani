package api

import (
	"crypto/hmac"
	"log"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"namakani/internal/auth"
)

const tgWebAppMaxAge = 24 * time.Hour

type tgInitData struct {
	UserID   int64
	AuthDate int64
}

func verifyTGInitData(token, initData string, now time.Time) (tgInitData, error) {
	var out tgInitData
	vals, err := url.ParseQuery(initData)
	if err != nil {
		return out, errors.New("bad init data")
	}
	gotHash := vals.Get("hash")
	if gotHash == "" {
		return out, errors.New("init data missing hash")
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(vals.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(sb.String()))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(gotHash))) {
		return out, errors.New("init data signature mismatch")
	}
	ad, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil {
		return out, errors.New("init data missing auth_date")
	}
	out.AuthDate = ad
	age := now.Sub(time.Unix(ad, 0))
	if age > tgWebAppMaxAge || age < -5*time.Minute {
		return out, errors.New("init data expired")
	}
	userRaw := vals.Get("user")
	if userRaw == "" {
		return out, errors.New("init data missing user")
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(userRaw), &u); err != nil || u.ID == 0 {
		return out, errors.New("init data bad user")
	}
	out.UserID = u.ID
	return out, nil
}

func tgInitDataFingerprint(initData string) string {
	sum := sha256.Sum256([]byte(initData))
	return hex.EncodeToString(sum[:])
}

// Records the fingerprint on first use and reports the claim as fresh; a
// repeat claim returns the hash of the session the first use issued.
// Rows outlive the verify window only until the next claim prunes them.
func (s *Server) claimTGInitData(fp string, authDate int64, now time.Time) (string, bool) {
	_, _ = s.st.DB.Exec(`DELETE FROM tg_webapp_logins WHERE auth_date < ?`, now.Add(-tgWebAppMaxAge).Unix())
	if _, err := s.st.DB.Exec(`INSERT INTO tg_webapp_logins(data_hash, auth_date, used_at) VALUES(?, ?, ?)`,
		fp, authDate, now.Unix()); err == nil {
		return "", true
	}
	var bound string
	if err := s.st.DB.QueryRow(`SELECT session_hash FROM tg_webapp_logins WHERE data_hash = ?`, fp).Scan(&bound); err != nil {
		return "", false
	}
	return bound, false
}

func (s *Server) bindTGInitDataSession(fp, token string) {
	_, _ = s.st.DB.Exec(`UPDATE tg_webapp_logins SET session_hash = ? WHERE data_hash = ?`,
		auth.TokenHash(token), fp)
}

func (s *Server) releaseTGInitData(fp string) {
	_, _ = s.st.DB.Exec(`DELETE FROM tg_webapp_logins WHERE data_hash = ?`, fp)
}

func (s *Server) recoverCredentialKey() ([]byte, bool) {
	if s.serverKey == nil {
		log.Printf("tg webapp-auth: recover key: no server key")
		return nil, false
	}
	var wrapped string
	err := s.st.DB.QueryRow(
		"SELECT wrapped_key FROM sessions WHERE wrapped_key <> '' AND expires_at > ? ORDER BY created_at DESC LIMIT 1",
		time.Now().Unix(),
	).Scan(&wrapped)
	if err != nil {
		log.Printf("tg webapp-auth: recover key: no unexpired session with a wrapped key")
		return nil, false
	}
	plain, err := auth.Decrypt(s.serverKey, wrapped)
	if err != nil || plain == "" {
		log.Printf("tg webapp-auth: recover key: decrypt failed: %v", err)
		return nil, false
	}
	return []byte(plain), true
}

func (s *Server) handleTelegramWebAppAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InitData string `json:"init_data"`
	}
	if err := decodeJSON(r, &req); err != nil || req.InitData == "" {
		log.Printf("tg webapp-auth: bad request from %s: init_data empty or invalid json", r.RemoteAddr)
		writeErr(w, http.StatusBadRequest, "缺少登录数据")
		return
	}
	log.Printf("tg webapp-auth: request from %s ua=%q init_data_len=%d", r.RemoteAddr, r.UserAgent(), len(req.InitData))
	cfg := s.tgLoadConfig()
	if cfg.Token == "" || cfg.ChatID == "" {
		log.Printf("tg webapp-auth: rejected: telegram not configured (token set=%v chat set=%v)", cfg.Token != "", cfg.ChatID != "")
		writeErr(w, http.StatusServiceUnavailable, "尚未配置 Telegram")
		return
	}
	init, err := verifyTGInitData(cfg.Token, req.InitData, time.Now())
	if err != nil {
		log.Printf("tg webapp-auth: verify failed: %v", err)
		writeErr(w, http.StatusUnauthorized, "Telegram 登录校验失败")
		return
	}
	if strconv.FormatInt(init.UserID, 10) != cfg.ChatID {
		log.Printf("tg webapp-auth: user mismatch: got %d, bound chat is %s", init.UserID, cfg.ChatID)
		writeErr(w, http.StatusForbidden, "Telegram 账号与绑定的聊天不一致")
		return
	}
	fp := tgInitDataFingerprint(req.InitData)
	bound, fresh := s.claimTGInitData(fp, init.AuthDate, time.Now())
	if !fresh {
		// A repeat is benign when it carries the live session this data
		// issued (a WebView reload re-running auth); anything else is a
		// replay and gets no session.
		if bound != "" {
			if tok := s.sessionToken(r); tok != "" && auth.TokenHash(tok) == bound {
				if _, ok := s.sessionKey(r); ok {
					s.renewSession(w, r)
					log.Printf("tg webapp-auth: repeat with its live session for user %d", init.UserID)
					writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
					return
				}
			}
		}
		log.Printf("tg webapp-auth: replayed init data rejected for user %d", init.UserID)
		writeErr(w, http.StatusUnauthorized, "Telegram 登录数据已使用，请重新打开小程序")
		return
	}
	claimed := true
	defer func() {
		if claimed {
			s.releaseTGInitData(fp)
		}
	}()
	key, ok := s.recoverCredentialKey()
	if !ok {
		log.Printf("tg webapp-auth: rejected: credential key not recoverable")
		writeErr(w, http.StatusConflict, "当前没有可用的登录会话，请先用主密码登录一次")
		return
	}
	token, err := s.startSession(w, r, key)
	if err != nil {
		log.Printf("tg webapp-auth: start session failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "会话处理失败")
		return
	}
	s.bindTGInitDataSession(fp, token)
	claimed = false
	s.startAllWorkers(key)
	log.Printf("tg webapp-auth: ok for user %d", init.UserID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
