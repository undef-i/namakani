package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"namakani/internal/auth"
	syncmgr "namakani/internal/sync"
)

const (
	settingTGEnabled  = "tg_enabled"
	settingTGChatID   = "tg_chat_id"
	settingTGTokenEnc = "tg_token_enc"
)

type tgConfig struct {
	Enabled bool
	ChatID  string
	Token   string
	HasTok  bool
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
	return cfg
}

func (c tgConfig) ready() bool {
	return c.Enabled && c.Token != "" && c.ChatID != ""
}

var tgHTTPClient = &http.Client{Timeout: 12 * time.Second}

func tgCall(token, method string, payload any) (json.RawMessage, error) {
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

func (s *Server) tgSupervise() {
	var cancel context.CancelFunc
	var running string
	for {
		token := s.tgLoadConfig().Token
		if token != running {
			if cancel != nil {
				cancel()
				cancel = nil
			}
			running = token
			if token != "" {
				ctx, c := context.WithCancel(context.Background())
				cancel = c
				go s.tgPoll(ctx, token)
			}
		}
		time.Sleep(5 * time.Second)
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
		"enabled":   cfg.Enabled,
		"chat_id":   cfg.ChatID,
		"has_token": cfg.HasTok,
	})
}

func (s *Server) handlePutTelegram(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		ChatID   string `json:"chat_id"`
		BotToken string `json:"bot_token"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	req.ChatID = strings.TrimSpace(req.ChatID)
	req.BotToken = strings.TrimSpace(req.BotToken)

	if req.BotToken != "" {
		if s.serverKey == nil {
			writeErr(w, http.StatusInternalServerError, "server key unavailable, cannot store token")
			return
		}
		enc, err := auth.Encrypt(s.serverKey, req.BotToken)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt token failed")
			return
		}
		if err := s.st.SetSetting(settingTGTokenEnc, enc); err != nil {
			writeErr(w, http.StatusInternalServerError, "store token failed")
			return
		}
	}
	if err := s.st.SetSetting(settingTGChatID, req.ChatID); err != nil {
		writeErr(w, http.StatusInternalServerError, "store chat id failed")
		return
	}
	enabled := "0"
	if req.Enabled {
		enabled = "1"
	}
	if err := s.st.SetSetting(settingTGEnabled, enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "store enabled flag failed")
		return
	}
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
		writeErr(w, http.StatusBadGateway, err.Error())
		return
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

const tgNamakaniBase = "https://namakani.example.com"

func tgDeepLink(id int64) string {
	return fmt.Sprintf("%s/m/%d", tgNamakaniBase, id)
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

func (s *Server) tgPushOne(cfg tgConfig, it tgItem) {
	_, err := tgCall(cfg.Token, "sendRichMessage", map[string]any{
		"chat_id":      cfg.ChatID,
		"rich_message": map[string]any{"blocks": tgRichBlocks(it)},
		"reply_markup": map[string]any{
			"inline_keyboard": [][]any{
				{
					map[string]any{
						"text":    "打开邮件",
						"web_app": map[string]any{"url": tgDeepLink(it.ID)},
					},
				},
			},
		},
	})
	if err != nil {
		log.Printf("namakani: telegram rich push failed: %v", err)
	}
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

func (s *Server) tgNotify(c syncmgr.Change) {
	cfg := s.tgLoadConfig()
	if !cfg.ready() {
		return
	}
	for _, it := range s.freshInboxItems(c) {
		go s.tgPushOne(cfg, it)
	}
}
