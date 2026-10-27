package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"namakani/internal/auth"
	syncmgr "namakani/internal/sync"
)

const (
	settingTGEnabled    = "tg_enabled"
	settingTGChatID     = "tg_chat_id"
	settingTGTokenEnc   = "tg_token_enc"
	settingTGPublicBase = "tg_public_base"
)

type tgConfig struct {
	Enabled    bool
	ChatID     string
	Token      string
	HasTok     bool
	PublicBase string
}

func (s *Server) tgLoadConfig() tgConfig {
	var cfg tgConfig
	if v, ok, _ := s.st.GetSetting(settingTGEnabled); ok && v == "1" {
		cfg.Enabled = true
	}
	if v, ok, _ := s.st.GetSetting(settingTGChatID); ok {
		cfg.ChatID = strings.TrimSpace(v)
	}
	if v, ok, _ := s.st.GetSetting(settingTGTokenEnc); ok && v != "" {
		cfg.HasTok = true
		if s.serverKey != nil {
			if tok, err := auth.Decrypt(s.serverKey, v); err == nil {
				cfg.Token = tok
			}
		}
	}
	if v, ok, _ := s.st.GetSetting(settingTGPublicBase); ok {
		cfg.PublicBase = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	return cfg
}

func tgNormalizePublicBase(v string) (string, bool) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", true
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return v, true
}

func (c tgConfig) ready() bool {
	return c.Enabled && c.Token != "" && c.ChatID != ""
}

var tgHTTPClient = &http.Client{Timeout: 12 * time.Second}

var tgCall = func(token, method string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := "https://api.telegram.org/bot" + token + "/" + method
	resp, err := tgHTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("telegram: bad response (HTTP %d)", resp.StatusCode)
	}
	if !env.OK {
		if env.Description != "" {
			return nil, fmt.Errorf("telegram: %s", env.Description)
		}
		return nil, fmt.Errorf("telegram: request failed (HTTP %d)", resp.StatusCode)
	}
	return env.Result, nil
}

func tgSendMessage(token, chatID, text string) error {
	_, err := tgCall(token, "sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	return err
}

type tgChat struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type tgUpdateMsg struct {
	Text string `json:"text"`
	Chat struct {
		ID        int64  `json:"id"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"chat"`
}

type tgUpdate struct {
	UpdateID     int64        `json:"update_id"`
	Message      *tgUpdateMsg `json:"message"`
	Edited       *tgUpdateMsg `json:"edited_message"`
	ChannelPost  *tgUpdateMsg `json:"channel_post"`
	MyChatMember *tgUpdateMsg `json:"my_chat_member"`
}

func (u *tgUpdate) anyMsg() *tgUpdateMsg {
	for _, m := range []*tgUpdateMsg{u.Message, u.Edited, u.ChannelPost, u.MyChatMember} {
		if m != nil {
			return m
		}
	}
	return nil
}

func tgChatOf(m *tgUpdateMsg) tgChat {
	title := m.Chat.Title
	if title == "" {
		title = strings.TrimSpace(m.Chat.FirstName + " " + m.Chat.LastName)
	}
	if m.Chat.Username != "" {
		if title != "" {
			title += " "
		}
		title += "@" + m.Chat.Username
	}
	return tgChat{ID: fmt.Sprint(m.Chat.ID), Type: m.Chat.Type, Title: title}
}

func (s *Server) tgReconcile() {
	s.tgMu.Lock()
	defer s.tgMu.Unlock()
	token := s.tgLoadConfig().Token
	if token == s.tgRunning {
		return
	}
	if s.tgCancel != nil {
		s.tgCancel()
		s.tgCancel = nil
	}
	s.tgRunning = token
	if token != "" {
		ctx, c := context.WithCancel(context.Background())
		s.tgCancel = c
		go s.tgPoll(ctx, token)
	}
}

func (s *Server) tgPoll(ctx context.Context, token string) {
	offset := s.tgBaseline(token)
	errStreak := 0
	for {
		raw, err := tgCallCtx(ctx, token, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         25,
			"allowed_updates": []string{"message", "edited_message", "channel_post", "my_chat_member"},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			errStreak++
			if errStreak == 1 {
				log.Printf("namakani: telegram poll failed: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		errStreak = 0
		var updates []tgUpdate
		if err := json.Unmarshal(raw, &updates); err != nil {
			continue
		}
		for i := range updates {
			u := &updates[i]
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			m := u.anyMsg()
			if m == nil {
				continue
			}
			chat := tgChatOf(m)
			s.tgSeenMu.Lock()
			s.tgSeen[chat.ID] = chat
			s.tgSeenMu.Unlock()
			if isBindCommand(m.Text) {
				if err := tgSendMessage(token, chat.ID, chat.ID); err != nil {
					log.Printf("namakani: telegram bind reply failed: %v", err)
				}
			}
		}
	}
}

func (s *Server) tgBaseline(token string) int64 {
	raw, err := tgCall(token, "getUpdates", map[string]any{"limit": 100})
	if err != nil {
		return 0
	}
	var updates []tgUpdate
	if err := json.Unmarshal(raw, &updates); err != nil {
		return 0
	}
	var max int64
	for _, u := range updates {
		if u.UpdateID > max {
			max = u.UpdateID
		}
	}
	return max + 1
}

func isBindCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if !strings.HasPrefix(t, "/") {
		return false
	}
	cmd := strings.Fields(t)[0]
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd == "/id" || cmd == "/bind" || cmd == "/start"
}

func tgCallCtx(ctx context.Context, token, method string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := "https://api.telegram.org/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 40 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("telegram: bad response (HTTP %d)", resp.StatusCode)
	}
	if !env.OK {
		if env.Description != "" {
			return nil, fmt.Errorf("telegram: %s", env.Description)
		}
		return nil, fmt.Errorf("telegram: request failed (HTTP %d)", resp.StatusCode)
	}
	return env.Result, nil
}

func (s *Server) handleGetTelegram(w http.ResponseWriter, r *http.Request) {
	cfg := s.tgLoadConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     cfg.Enabled,
		"chat_id":     cfg.ChatID,
		"has_token":   cfg.HasTok,
		"public_base": cfg.PublicBase,
	})
}

func (s *Server) handlePutTelegram(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled    bool   `json:"enabled"`
		ChatID     string `json:"chat_id"`
		BotToken   string `json:"bot_token"`
		PublicBase string `json:"public_base"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	req.ChatID = strings.TrimSpace(req.ChatID)
	req.BotToken = strings.TrimSpace(req.BotToken)
	publicBase, ok := tgNormalizePublicBase(req.PublicBase)
	if !ok {
		writeErr(w, http.StatusBadRequest, "公开地址格式不对，应以 http:// 或 https:// 开头，例如 https://mail.example.com")
		return
	}

	if req.BotToken != "" {
		if s.serverKey == nil {
			writeErr(w, http.StatusInternalServerError, "服务器密钥不可用，无法保存令牌")
			return
		}
		enc, err := auth.Encrypt(s.serverKey, req.BotToken)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "令牌加密失败")
			return
		}
		if err := s.st.SetSetting(settingTGTokenEnc, enc); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存令牌失败")
			return
		}
	}
	if err := s.st.SetSetting(settingTGChatID, req.ChatID); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存聊天编号失败")
		return
	}
	if err := s.st.SetSetting(settingTGPublicBase, publicBase); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存公开地址失败")
		return
	}
	enabled := "0"
	if req.Enabled {
		enabled = "1"
	}
	if err := s.st.SetSetting(settingTGEnabled, enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存开关失败")
		return
	}
	s.tgReconcile()
	s.handleGetTelegram(w, r)
}

func (s *Server) handleTelegramDiscover(w http.ResponseWriter, r *http.Request) {
	s.tgSeenMu.Lock()
	chats := make([]tgChat, 0, len(s.tgSeen))
	for _, c := range s.tgSeen {
		chats = append(chats, c)
	}
	s.tgSeenMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	_ = decodeJSON(r, &req)
	cfg := s.tgLoadConfig()
	token := strings.TrimSpace(req.BotToken)
	if token == "" {
		token = cfg.Token
	}
	chatID := strings.TrimSpace(req.ChatID)
	if chatID == "" {
		chatID = cfg.ChatID
	}
	if token == "" || chatID == "" {
		writeErr(w, http.StatusBadRequest, "先填写 Bot Token 和 Chat ID")
		return
	}
	if err := tgSendMessage(token, chatID, chatID); err != nil {
		writeUserErr(w, http.StatusBadGateway, "Telegram 发送失败，请检查 Bot Token 和 Chat ID", err)
		return
	}
	if cfg.PublicBase != "" {
		if _, err := tgCall(token, "setChatMenuButton", map[string]any{
			"chat_id": chatID,
			"menu_button": map[string]any{
				"type":    "web_app",
				"text":    "邮箱",
				"web_app": map[string]any{"url": cfg.PublicBase},
			},
		}); err != nil {
			log.Printf("tg setChatMenuButton failed: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type tgItem struct {
	ID      int64
	DateTS  int64
	Account string
	From    string
	Subject string
	Snippet string
}

func tgDeepLink(base string, id int64) string {
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/m/%d", base, id)
}

func rtBold(s string) any {
	return map[string]any{"type": "bold", "text": s}
}

func rtItalic(s string) any {
	return map[string]any{"type": "italic", "text": s}
}

var tgDisplayZone = time.FixedZone("CST", 8*3600)

func rtDateTime(ts int64) any {
	return map[string]any{
		"type":      "date_time",
		"text":      time.Unix(ts, 0).In(tgDisplayZone).Format("1月2日 15:04"),
		"unix_time": ts,
	}
}

func tgParagraph(text any) map[string]any {
	return map[string]any{"type": "paragraph", "text": text}
}

func tgRichBlocks(it tgItem) []any {
	blocks := []any{
		tgParagraph([]any{rtBold(it.Subject)}),
		tgParagraph([]any{it.From, "  ", rtDateTime(it.DateTS)}),
	}
	blocks = append(blocks, map[string]any{"type": "footer", "text": tgFooterText(it)})
	return blocks
}

func tgFooterText(it tgItem) any {
	if it.Snippet == "" {
		return it.Account
	}
	return []any{rtItalic(it.Snippet), "\n", it.Account}
}

func (s *Server) tgPushedIDs(ids []int64) map[int64]bool {
	out := make(map[int64]bool)
	if len(ids) == 0 {
		return out
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.st.DB.Query(`SELECT message_id FROM tg_pushes WHERE message_id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		log.Printf("namakani: telegram push ledger lookup failed: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out[id] = true
		}
	}
	return out
}

func (s *Server) tgRecordPush(id int64) {
	if _, err := s.st.DB.Exec(`INSERT OR IGNORE INTO tg_pushes(message_id, pushed_at) VALUES(?, ?)`,
		id, time.Now().Unix()); err != nil {
		log.Printf("namakani: telegram push ledger record failed for message %d: %v", id, err)
	}
}

func (s *Server) tgPushOne(cfg tgConfig, it tgItem) {
	payload := map[string]any{
		"chat_id":      cfg.ChatID,
		"rich_message": map[string]any{"blocks": tgRichBlocks(it)},
	}
	if link := tgDeepLink(cfg.PublicBase, it.ID); link != "" {
		payload["reply_markup"] = map[string]any{
			"inline_keyboard": [][]any{
				{
					map[string]any{
						"text":    "打开邮件",
						"web_app": map[string]any{"url": link},
					},
				},
			},
		}
	}
	_, err := tgCall(cfg.Token, "sendRichMessage", payload)
	if err != nil {
		log.Printf("namakani: telegram rich push failed: %v", err)
		return
	}
	s.tgRecordPush(it.ID)
}

func (s *Server) tgArrivedInboxIDs(c syncmgr.Change) map[int64]bool {
	out := make(map[int64]bool)
	if len(c.Inserted) == 0 {
		return out
	}
	ph := make([]string, len(c.Inserted))
	args := make([]any, 0, len(c.Inserted)+1)
	args = append(args, c.AccountID)
	for i, id := range c.Inserted {
		ph[i] = "?"
		args = append(args, id)
	}
	rows, err := s.st.DB.Query(`SELECT message_id FROM first_seen WHERE account_id = ? AND folder_special = 'inbox'
		AND message_id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		log.Printf("namakani: telegram arrival lookup failed: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out[id] = true
		}
	}
	return out
}

func (s *Server) freshInboxItems(c syncmgr.Change) []tgItem {
	if len(c.Inserted) == 0 {
		return nil
	}
	f, ok := s.folderRow(c.FolderID)
	if !ok || f.SpecialUse != "inbox" {
		return nil
	}
	var cutoff int64
	if c.Initial {
		cutoff = time.Now().Add(-time.Duration(s.st.AppSettings().PushWindowMinutes) * time.Minute).Unix()
	}
	var items []tgItem
	for _, row := range s.messageRowsByIDs(c.Inserted) {
		if row.FolderID != c.FolderID || row.DateTS < cutoff {
			continue
		}
		from := row.FromName
		if from == "" {
			from = row.FromEmail
		} else if row.FromEmail != "" {
			from += " <" + row.FromEmail + ">"
		}
		subject := row.Subject
		if subject == "" {
			subject = "(无主题)"
		}
		items = append(items, tgItem{
			ID:      row.ID,
			DateTS:  row.DateTS,
			Account: row.AccountLabel,
			From:    from,
			Subject: subject,
			Snippet: row.Snippet,
		})
	}
	return items
}

func (s *Server) arrivalInboxItems(c syncmgr.Change) []tgItem {
	if len(c.Inserted) == 0 {
		return nil
	}
	arrived := s.tgArrivedInboxIDs(c)
	if len(arrived) == 0 {
		return nil
	}
	var items []tgItem
	for _, row := range s.messageRowsByIDs(c.Inserted) {
		if !arrived[row.ID] {
			continue
		}
		from := row.FromName
		if from == "" {
			from = row.FromEmail
		} else if row.FromEmail != "" {
			from += " <" + row.FromEmail + ">"
		}
		subject := row.Subject
		if subject == "" {
			subject = "(无主题)"
		}
		items = append(items, tgItem{
			ID:      row.ID,
			DateTS:  row.DateTS,
			Account: row.AccountLabel,
			From:    from,
			Subject: subject,
			Snippet: row.Snippet,
		})
	}
	return items
}

func (s *Server) tgNotify(c syncmgr.Change) {
	cfg := s.tgLoadConfig()
	if !cfg.ready() {
		return
	}
	items := s.arrivalInboxItems(c)
	if len(items) == 0 {
		return
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	pushed := s.tgPushedIDs(ids)
	for _, it := range items {
		if pushed[it.ID] {
			continue
		}
		go s.tgPushOne(cfg, it)
	}
}
