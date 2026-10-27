package store

import (
	"fmt"
	"strconv"
	"strings"
)

type AppSettings struct {
	CacheBudgetGB     int64  `json:"cache_budget_gb"`
	SyncBatch         int    `json:"sync_batch"`
	MaxAttachmentMB   int64  `json:"max_attachment_mb"`
	PushWindowMinutes int64  `json:"push_window_minutes"`
	BaseURL           string `json:"base_url"`
}

func DefaultAppSettings() AppSettings {
	return AppSettings{
		CacheBudgetGB:     4,
		SyncBatch:         100,
		MaxAttachmentMB:   20,
		PushWindowMinutes: 15,
	}
}

const (
	settingCacheBudgetGB = "app_cache_budget_gb"
	settingSyncBatch     = "app_sync_batch"
	settingMaxAttachMB   = "app_max_attachment_mb"
	settingPushWindowMin = "app_push_window_minutes"
	settingBaseURL       = "base_url"
)

func (s *Store) loadAppSettings() {
	cfg := DefaultAppSettings()
	read := func(key string, dst *int64) {
		if v, ok, err := s.GetSetting(key); err == nil && ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*dst = n
			}
		}
	}
	read(settingCacheBudgetGB, &cfg.CacheBudgetGB)
	read(settingMaxAttachMB, &cfg.MaxAttachmentMB)
	read(settingPushWindowMin, &cfg.PushWindowMinutes)
	var batch int64
	read(settingSyncBatch, &batch)
	if batch > 0 {
		cfg.SyncBatch = int(batch)
	}
	if v, ok, err := s.GetSetting(settingBaseURL); err == nil && ok {
		cfg.BaseURL = v
	}
	s.appMu.Lock()
	s.app = cfg
	s.appMu.Unlock()
}

func (s *Store) AppSettings() AppSettings {
	s.appMu.RLock()
	defer s.appMu.RUnlock()
	return s.app
}

func (s *Store) SaveAppSettings(cfg AppSettings) error {
	for key, n := range map[string]int64{
		settingCacheBudgetGB: cfg.CacheBudgetGB,
		settingSyncBatch:     int64(cfg.SyncBatch),
		settingMaxAttachMB:   cfg.MaxAttachmentMB,
		settingPushWindowMin: cfg.PushWindowMinutes,
	} {
		if err := s.SetSetting(key, strconv.FormatInt(n, 10)); err != nil {
			return fmt.Errorf("save setting %s: %w", key, err)
		}
	}
	if err := s.SetSetting(settingBaseURL, cfg.BaseURL); err != nil {
		return fmt.Errorf("save setting %s: %w", settingBaseURL, err)
	}
	s.appMu.Lock()
	s.app = cfg
	s.appMu.Unlock()
	return nil
}

func (s *Store) BaseURL() string {
	return strings.TrimRight(strings.TrimSpace(s.AppSettings().BaseURL), "/")
}

func (s *Store) CacheBudgetBytes() int64 {
	return s.AppSettings().CacheBudgetGB << 30
}

func (s *Store) CacheUsedBytes() (int64, error) {
	bodies, atts, err := s.CacheUsage()
	return bodies + atts, err
}
