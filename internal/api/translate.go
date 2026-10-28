package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"namakani/internal/auth"
	"namakani/internal/store"
	"namakani/internal/translate"
)

const (
	settingTrEngine     = "tr_engine"
	settingTrTarget     = "tr_target"
	settingTrLLMFormat  = "tr_llm_format"
	settingTrLLMEndpt   = "tr_llm_endpoint"
	settingTrLLMModel   = "tr_llm_model"
	settingTrLLMKeyEnc  = "tr_llm_key_enc"
	defaultTrEngine     = "google"
	defaultTrLLMFormat  = "openai"
	translateTimeout    = 180 * time.Second
	avatarNegativeTTL   = 7 * 24 * time.Hour
	avatarResolveWindow = 8 * time.Second
)

type translateSettings struct {
	Engine      string `json:"engine"`
	Target      string `json:"target"`
	LLMFormat   string `json:"llm_format"`
	LLMEndpoint string `json:"llm_endpoint"`
	LLMModel    string `json:"llm_model"`
	HasKey      bool   `json:"has_key"`
}

func (s *Server) loadTranslateSettings() (translateSettings, string) {
	cfg := translateSettings{Engine: defaultTrEngine, Target: "auto", LLMFormat: defaultTrLLMFormat}
	if v, ok, _ := s.st.GetSetting(settingTrEngine); ok && v != "" {
		cfg.Engine = v
	}
	if cfg.Engine == "local" {
		cfg.Engine = defaultTrEngine
		_ = s.st.SetSetting(settingTrEngine, defaultTrEngine)
	}
	if v, ok, _ := s.st.GetSetting(settingTrTarget); ok && v != "" {
		cfg.Target = v
	}
	if v, ok, _ := s.st.GetSetting(settingTrLLMFormat); ok && v != "" {
		cfg.LLMFormat = v
	}
	if v, ok, _ := s.st.GetSetting(settingTrLLMEndpt); ok {
		cfg.LLMEndpoint = v
	}
	if v, ok, _ := s.st.GetSetting(settingTrLLMModel); ok {
		cfg.LLMModel = v
	}
	key := ""
	if v, ok, _ := s.st.GetSetting(settingTrLLMKeyEnc); ok && v != "" {
		cfg.HasKey = true
		if s.serverKey != nil {
			if plain, err := auth.Decrypt(s.serverKey, v); err == nil {
				key = plain
			}
		}
	}
	return cfg, key
}

func (s *Server) handleGetTranslateSettings(w http.ResponseWriter, r *http.Request) {
	cfg, _ := s.loadTranslateSettings()
	writeJSON(w, http.StatusOK, map[string]any{"settings": cfg})
}

func (s *Server) handlePutTranslateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		translateSettings
		LLMAPIKey string `json:"llm_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "设置内容不对")
		return
	}
	switch req.Engine {
	case "google", "llm":
	default:
		writeErr(w, http.StatusBadRequest, "翻译引擎不可用")
		return
	}
	if req.LLMFormat == "" {
		req.LLMFormat = defaultTrLLMFormat
	}
	switch req.LLMFormat {
	case "openai", "responses", "anthropic":
	default:
		writeErr(w, http.StatusBadRequest, "翻译服务格式不对")
		return
	}
	for key, val := range map[string]string{
		settingTrEngine:    req.Engine,
		settingTrTarget:    req.Target,
		settingTrLLMFormat: req.LLMFormat,
		settingTrLLMEndpt:  strings.TrimSpace(req.LLMEndpoint),
		settingTrLLMModel:  strings.TrimSpace(req.LLMModel),
	} {
		if err := s.st.SetSetting(key, val); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存设置失败")
			return
		}
	}
	if req.LLMAPIKey != "" {
		if s.serverKey == nil {
			writeErr(w, http.StatusInternalServerError, "服务器密钥不可用")
			return
		}
		enc, err := auth.Encrypt(s.serverKey, req.LLMAPIKey)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "密钥加密失败")
			return
		}
		if err := s.st.SetSetting(settingTrLLMKeyEnc, enc); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存设置失败")
			return
		}
	}
	s.trMgr.CloseWorker()
	cfg, _ := s.loadTranslateSettings()
	writeJSON(w, http.StatusOK, map[string]any{"settings": cfg})
}

func (s *Server) buildTranslateEngine(cfg translateSettings, key string) (translate.Engine, error) {
	if s.translateBuild != nil {
		return s.translateBuild(cfg)
	}
	switch cfg.Engine {
	case "google":
		return translate.NewGoogleEngine(), nil
	case "llm":
		if cfg.LLMEndpoint == "" || cfg.LLMModel == "" {
			return nil, fmt.Errorf("llm endpoint or model not configured")
		}
		return translate.NewLLMEngine(cfg.LLMFormat, cfg.LLMEndpoint, cfg.LLMModel, key), nil
	}
	return nil, fmt.Errorf("unknown translate engine %q", cfg.Engine)
}

type localResolverEngine struct {
	mgr *translate.Manager
}

func (e *localResolverEngine) Key() string { return "local" }

func (e *localResolverEngine) Translate(ctx context.Context, segs []translate.Segment, src, tgt string) ([]translate.Segment, error) {
	eng, err := e.mgr.EngineFor(src, tgt)
	if err != nil {
		return nil, err
	}
	return eng.Translate(ctx, segs, src, tgt)
}

func (s *Server) translateEngineKey(cfg translateSettings, key, src, target string) (string, error) {
	switch cfg.Engine {
	case "google":
		return "google", nil
	case "llm":
		return "llm:" + cfg.LLMFormat + ":" + cfg.LLMModel, nil
	}
	return "", fmt.Errorf("unknown translate engine %q", cfg.Engine)
}

type translateResult struct {
	Subject      string `json:"subject"`
	HTML         string `json:"html"`
	Text         string `json:"text"`
	SrcLang      string `json:"src_lang"`
	Cached       bool   `json:"cached"`
	SameLanguage bool   `json:"same_language"`
}

func (s *Server) handleTranslateMessage(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
		return
	}
	var req struct {
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	cfg, rawKey := s.loadTranslateSettings()
	target := req.Target
	if target == "" {
		target = cfg.Target
	}
	if target == "" || target == "auto" {
		writeErr(w, http.StatusBadRequest, "请选择目标语言")
		return
	}
	var subject string
	if err := s.st.DB.QueryRow("SELECT subject FROM messages WHERE id = ?", id).Scan(&subject); err != nil {
		writeErr(w, http.StatusNotFound, "邮件不存在")
		return
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "定位邮件失败")
		return
	}
	text, rawHTML, atts, _, err := s.ensureBody(key, id, route)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "正文加载失败，请稍后重试", err)
		return
	}
	cidURLs := map[string]string{}
	for _, a := range atts {
		if a.ContentID != "" {
			cidURLs[a.ContentID] = "/api/messages/" + strconv.FormatInt(id, 10) +
				"/attachments/" + strconv.FormatInt(a.ID, 10)
		}
	}
	sanitized := sanitizeMailHTML(rawHTML, cidURLs)
	src := translate.Detect(subject + "\n" + text)
	if src == "" && sanitized != "" {
		src = translate.Detect(translate.StripTagsForDetect(sanitized))
	}
	if src == target {
		writeJSON(w, http.StatusOK, translateResult{
			Subject: subject, HTML: sanitized, Text: text, SrcLang: src, SameLanguage: true,
		})
		return
	}
	engineKey, err := s.translateEngineKey(cfg, rawKey, src, target)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "翻译服务不可用，请检查翻译设置", err)
		return
	}
	if row, ok, err := s.st.GetTranslation(id, target, engineKey); err == nil && ok {
		writeJSON(w, http.StatusOK, translateResult{
			Subject: row.Subject, HTML: row.HTML, Text: row.Text, SrcLang: row.SrcLang, Cached: true,
		})
		return
	}
	eng, err := s.buildTranslateEngine(cfg, rawKey)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "翻译服务不可用，请检查翻译设置", err)
		return
	}
	svc := translate.NewService(eng)
	ctx, cancel := context.WithTimeout(r.Context(), translateTimeout)
	defer cancel()
	trSubject := subject
	if strings.TrimSpace(subject) != "" {
		segs, err := svc.TranslateSegments(ctx, []translate.Segment{{ID: 1, Text: subject}}, src, target)
		if err != nil {
			writeUserErr(w, http.StatusBadGateway, "翻译失败，请稍后重试", err)
			return
		}
		trSubject = segs[0].Text
	}
	trHTML := ""
	trText := ""
	if sanitized != "" {
		trHTML, err = svc.TranslateHTML(ctx, sanitized, src, target)
		if err != nil {
			writeUserErr(w, http.StatusBadGateway, "翻译失败，请稍后重试", err)
			return
		}
	} else if text != "" {
		trText, err = svc.TranslateText(ctx, text, src, target)
		if err != nil {
			writeUserErr(w, http.StatusBadGateway, "翻译失败，请稍后重试", err)
			return
		}
	}
	row := &store.TranslationRow{Subject: trSubject, HTML: trHTML, Text: trText, SrcLang: src}
	if err := s.st.PutTranslation(id, target, engineKey, row); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入缓存失败")
		return
	}
	writeJSON(w, http.StatusOK, translateResult{
		Subject: trSubject, HTML: trHTML, Text: trText, SrcLang: src,
	})
}

func (s *Server) handleLLMModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LLMFormat   string `json:"llm_format"`
		LLMEndpoint string `json:"llm_endpoint"`
		LLMAPIKey   string `json:"llm_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	format := req.LLMFormat
	if format == "" {
		format = defaultTrLLMFormat
	}
	switch format {
	case "openai", "responses", "anthropic":
	default:
		writeErr(w, http.StatusBadRequest, "翻译服务格式不对")
		return
	}
	endpoint := strings.TrimSpace(req.LLMEndpoint)
	if endpoint == "" {
		writeErr(w, http.StatusBadRequest, "请填写翻译服务地址")
		return
	}
	key := req.LLMAPIKey
	if key == "" {
		_, saved := s.loadTranslateSettings()
		key = saved
	}
	eng := translate.NewLLMEngine(format, endpoint, "", key)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ids, err := eng.ListModels(ctx)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "读取模型列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

func (s *Server) handleTranslateLanguages(w http.ResponseWriter, r *http.Request) {
	s.trLangsMu.Lock()
	langs := s.trLangs
	s.trLangsMu.Unlock()
	if langs == nil {
		fetch := s.trLangsFetch
		if fetch == nil {
			fetch = func(ctx context.Context) ([]translate.Language, error) {
				return translate.FetchGoogleLanguages(ctx, nil, translate.GoogleLanguagesURL)
			}
		}
		fetched, err := fetch(r.Context())
		if err != nil {
			writeUserErr(w, http.StatusBadGateway, "读取语言名单失败，请稍后重试", err)
			return
		}
		s.trLangsMu.Lock()
		if s.trLangs == nil {
			s.trLangs = fetched
		}
		langs = s.trLangs
		s.trLangsMu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"languages": langs})
}

func (s *Server) handleTranslateModels(w http.ResponseWriter, r *http.Request) {
	pairs, err := s.trMgr.Pairs()
	if err != nil {
		writeUserErr(w, http.StatusInternalServerError, "读取翻译设置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pairs": pairViews(pairs)})
}

func pairViews(pairs []translate.Pair) []map[string]any {
	out := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, map[string]any{
			"id": p.ID, "from": p.From, "to": p.To, "disk_bytes": p.DiskBytes,
		})
	}
	return out
}
