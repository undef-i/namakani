package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"namakani/internal/auth"
	syncmgr "namakani/internal/sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const (
	settingVAPIDPublic    = "vapid_public"
	settingVAPIDPrivateEn = "vapid_private_enc"

	wpSubscriber = "mailto:admin@example.com"
)

func (s *Server) vapidKeys() (pub, priv string, err error) {
	if s.serverKey == nil {
		return "", "", fmt.Errorf("server key unavailable")
	}
	if v, ok, _ := s.st.GetSetting(settingVAPIDPublic); ok && v != "" {
		pub = v
		enc, ok2, _ := s.st.GetSetting(settingVAPIDPrivateEn)
		if !ok2 || enc == "" {
			return "", "", fmt.Errorf("vapid private key missing")
		}
		priv, err = auth.Decrypt(s.serverKey, enc)
		return pub, priv, err
	}
	priv, pub, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	enc, err := auth.Encrypt(s.serverKey, priv)
	if err != nil {
		return "", "", err
	}
	if err := s.st.SetSetting(settingVAPIDPublic, pub); err != nil {
		return "", "", err
	}
	if err := s.st.SetSetting(settingVAPIDPrivateEn, enc); err != nil {
		return "", "", err
	}
	return pub, priv, nil
}

type wpSubscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

func (s *Server) wpSubscriptions() []wpSubscription {
	rows, err := s.st.DB.Query("SELECT endpoint, p256dh, auth FROM push_subscriptions")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []wpSubscription
	for rows.Next() {
		var sub wpSubscription
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth); err != nil {
			break
		}
		out = append(out, sub)
	}
	return out
}

func (s *Server) wpDeleteSubscription(endpoint string) {
	_, _ = s.st.DB.Exec("DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
}

func (s *Server) wpSend(pub, priv string, subs []wpSubscription, payload []byte) (sent int) {
	opts := &webpush.Options{
		Subscriber:      wpSubscriber,
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
		TTL:             3600,
	}
	for _, sub := range subs {
		resp, err := webpush.SendNotification(payload, &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
		}, opts)
		if err != nil {
			log.Printf("namakani: web push send: %v", err)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			s.wpDeleteSubscription(sub.Endpoint)
		case http.StatusCreated, http.StatusOK:
			sent++
		default:
			log.Printf("namakani: web push: push service answered %d for %s", resp.StatusCode, sub.Endpoint)
		}
	}
	return sent
}

func (s *Server) wpNotify(c syncmgr.Change) {
	subs := s.wpSubscriptions()
	if len(subs) == 0 {
		return
	}
	items := s.freshInboxItems(c)
	if len(items) == 0 {
		return
	}
	pub, priv, err := s.vapidKeys()
	if err != nil {
		log.Printf("namakani: web push: vapid keys: %v", err)
		return
	}
	go func() {
		for _, it := range items {
			payload, err := json.Marshal(map[string]any{
				"title": it.Subject,
				"body":  it.From,
				"url":   tgDeepLink(it.ID),
				"id":    it.ID,
			})
			if err != nil {
				continue
			}
			s.wpSend(pub, priv, subs, payload)
		}
	}()
}

func (s *Server) handleGetWebPush(w http.ResponseWriter, r *http.Request) {
	pub, _, err := s.vapidKeys()
	if err != nil {
		writeUserErr(w, http.StatusInternalServerError, "推送密钥不可用", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"public_key":    pub,
		"subscriptions": len(s.wpSubscriptions()),
	})
}

type wpSubscribeReq struct {
	Endpoint       string `json:"endpoint"`
	ExpirationTime *int64 `json:"expirationTime"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req wpSubscribeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if !strings.HasPrefix(req.Endpoint, "https://") || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		writeErr(w, http.StatusBadRequest, "订阅参数不完整")
		return
	}
	_, err := s.st.DB.Exec(
		`INSERT INTO push_subscriptions(endpoint, p256dh, auth, created_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth`,
		req.Endpoint, req.Keys.P256dh, req.Keys.Auth, time.Now().Unix())
	if err != nil {
		writeUserErr(w, http.StatusInternalServerError, "保存推送订阅失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": len(s.wpSubscriptions())})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Endpoint == "" {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	s.wpDeleteSubscription(req.Endpoint)
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": len(s.wpSubscriptions())})
}

func (s *Server) handleWebPushTest(w http.ResponseWriter, r *http.Request) {
	subs := s.wpSubscriptions()
	if len(subs) == 0 {
		writeErr(w, http.StatusBadRequest, "还没有任何浏览器订阅")
		return
	}
	pub, priv, err := s.vapidKeys()
	if err != nil {
		writeUserErr(w, http.StatusInternalServerError, "推送密钥不可用", err)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"title": "namakani",
		"body":  "测试推送",
		"url":   tgNamakaniBase + "/",
	})
	sent := s.wpSend(pub, priv, subs, payload)
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent, "subscriptions": len(s.wpSubscriptions())})
}
