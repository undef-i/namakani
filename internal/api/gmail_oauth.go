package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"namakani/internal/auth"
	"namakani/internal/gmail"
	"namakani/internal/store"
)

const (
	settingGmailOAuthClientID  = "gmail_oauth_client_id"
	settingGmailOAuthSecretEnc = "gmail_oauth_client_secret_enc"
)

type gmailOAuthConfig struct {
	ClientID     string
	ClientSecret string
	HasSecret    bool
	PubSubTopic  string
	PubSubToken  string
}

func (s *Server) gmailOAuthLoad() gmailOAuthConfig {
	var cfg gmailOAuthConfig
	if v, ok, _ := s.st.GetSetting(settingGmailOAuthClientID); ok {
		cfg.ClientID = strings.TrimSpace(v)
	}
	if v, ok, _ := s.st.GetSetting(settingGmailOAuthSecretEnc); ok && v != "" {
		cfg.HasSecret = true
		if s.serverKey != nil {
			if sec, err := auth.Decrypt(s.serverKey, v); err == nil {
				cfg.ClientSecret = sec
			}
		}
	}
	if v, ok, _ := s.st.GetSetting(store.SettingGmailPubSubTopic); ok {
		cfg.PubSubTopic = strings.TrimSpace(v)
	}
	if v, ok, _ := s.st.GetSetting(store.SettingGmailPubSubTokenEnc); ok && v != "" {
		if s.serverKey != nil {
			if tok, err := auth.Decrypt(s.serverKey, v); err == nil {
				cfg.PubSubToken = tok
			}
		}
	}
	return cfg
}

func (s *Server) gmailPubSubSave(topic string) error {
	if err := s.st.SetSetting(store.SettingGmailPubSubTopic, topic); err != nil {
		return err
	}
	if topic == "" {
		return nil
	}
	cfg := s.gmailOAuthLoad()
	if cfg.PubSubToken != "" {
		return nil
	}
	if s.serverKey == nil {
		return errors.New("server key unavailable")
	}
	token, err := gmail.RandomState()
	if err != nil {
		return err
	}
	enc, err := auth.Encrypt(s.serverKey, token)
	if err != nil {
		return err
	}
	return s.st.SetSetting(store.SettingGmailPubSubTokenEnc, enc)
}

func (s *Server) gmailPubSubEndpoint() string {
	cfg := s.gmailOAuthLoad()
	base := s.st.BaseURL()
	if base == "" || cfg.PubSubTopic == "" || cfg.PubSubToken == "" {
		return ""
	}
	return base + "/api/gmail/pubsub?token=" + cfg.PubSubToken
}

func (s *Server) gmailOAuthSave(clientID, clientSecret string) error {
	if err := s.st.SetSetting(settingGmailOAuthClientID, clientID); err != nil {
		return err
	}
	if clientSecret != "" {
		if s.serverKey == nil {
			return errors.New("server key unavailable")
		}
		enc, err := auth.Encrypt(s.serverKey, clientSecret)
		if err != nil {
			return err
		}
		if err := s.st.SetSetting(settingGmailOAuthSecretEnc, enc); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleGetGmailOAuth(w http.ResponseWriter, r *http.Request) {
	cfg := s.gmailOAuthLoad()
	writeJSON(w, http.StatusOK, map[string]any{
		"client_id":       cfg.ClientID,
		"has_secret":      cfg.HasSecret,
		"pubsub_topic":    cfg.PubSubTopic,
		"pubsub_endpoint": s.gmailPubSubEndpoint(),
	})
}

func (s *Server) handlePutGmailOAuth(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		PubSubTopic  string `json:"pubsub_topic"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := s.gmailOAuthSave(strings.TrimSpace(in.ClientID), in.ClientSecret); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	topic := strings.TrimSpace(in.PubSubTopic)
	if topic != s.gmailOAuthLoad().PubSubTopic {
		if err := s.gmailPubSubSave(topic); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if key, ok := s.credentialKey(r); ok {
			rows, err := s.st.DB.Query("SELECT id FROM accounts WHERE kind = 'gmail'")
			if err == nil {
				var ids []int64
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err == nil {
						ids = append(ids, id)
					}
				}
				rows.Close()
				for _, id := range ids {
					s.startWorkerFor(id, key)
				}
			}
		}
	}
	s.handleGetGmailOAuth(w, r)
}

func (s *Server) gmailAccountPassword(id int64, key []byte, stored string) (string, error) {
	var creds gmail.Creds
	if err := json.Unmarshal([]byte(stored), &creds); err != nil {
		return "", err
	}
	shared := s.gmailOAuthLoad()
	if creds.ClientID == "" && creds.ClientSecret == "" {
		creds.ClientID = shared.ClientID
		creds.ClientSecret = shared.ClientSecret
		return creds.JSON(), nil
	}
	clientID := shared.ClientID
	if clientID == "" {
		clientID = creds.ClientID
	}
	secret := shared.ClientSecret
	if secret == "" {
		secret = creds.ClientSecret
	}
	if clientID != shared.ClientID || secret != shared.ClientSecret {
		if err := s.gmailOAuthSave(clientID, secret); err != nil {
			log.Printf("gmail: migrating account %d oauth client to shared settings failed: %v", id, err)
		}
	}
	if enc, err := auth.Encrypt(key, gmail.TokenCredsJSON(creds.RefreshToken)); err == nil {
		if _, err := s.st.DB.Exec(`UPDATE accounts SET enc_password = ? WHERE id = ? AND kind = 'gmail'`, enc, id); err != nil {
			log.Printf("gmail: rewriting account %d credentials to token-only failed: %v", id, err)
		}
	} else {
		log.Printf("gmail: rewriting account %d credentials to token-only failed: %v", id, err)
	}
	creds.ClientID = clientID
	creds.ClientSecret = secret
	return creds.JSON(), nil
}
