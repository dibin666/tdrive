package config

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// RuntimeSettings are the settings that may be changed from the WebUI. The
// deployment settings (data directory, local source directory, listen address,
// public URL and the first administrator) remain environment-only because
// changing them would require moving files, rebinding the server or changing
// the current account.
type RuntimeSettings struct {
	AppID             int
	AppHash           string
	SegmentSize       int64
	PoolSize          int64
	UploadThreads     int
	StreamConcurrency int
	WebDAVEnabled     bool
	LogLevel          string
}

// RuntimeConfig keeps WebUI changes safe while uploads and downloads are
// running. Services take a short snapshot before starting an operation instead
// of holding a lock over network I/O.
type RuntimeConfig struct {
	mu       sync.RWMutex
	settings RuntimeSettings
}

func NewRuntimeConfig(settings RuntimeSettings) *RuntimeConfig {
	return &RuntimeConfig{settings: settings}
}

func (r *RuntimeConfig) Snapshot() RuntimeSettings {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.settings
}

func (r *RuntimeConfig) Set(settings RuntimeSettings) {
	r.mu.Lock()
	r.settings = settings
	r.mu.Unlock()
}

// RuntimeSettings returns the current WebUI-adjustable values. The fallback is
// useful for tests and for callers that construct Config literals directly.
func (c *Config) RuntimeSettings() RuntimeSettings {
	if c.Runtime != nil {
		return c.Runtime.Snapshot()
	}
	return RuntimeSettings{
		AppID:             c.Telegram.AppID,
		AppHash:           c.Telegram.AppHash,
		SegmentSize:       c.Storage.SegmentSize,
		PoolSize:          c.Telegram.PoolSize,
		UploadThreads:     c.Telegram.UploadThreads,
		StreamConcurrency: c.Stream.Concurrency,
		WebDAVEnabled:     c.WebDAV.Enabled,
		LogLevel:          c.LogLevel,
	}
}

// SetRuntimeSettings publishes a new WebUI-adjustable snapshot.
func (c *Config) SetRuntimeSettings(settings RuntimeSettings) {
	if c.Runtime == nil {
		c.Runtime = NewRuntimeConfig(settings)
		return
	}
	c.Runtime.Set(settings)
}

// Validate checks the values that can be changed at runtime.
func (s RuntimeSettings) Validate() error {
	switch {
	case s.AppID < 0:
		return fmt.Errorf("telegram app id must not be negative")
	case s.SegmentSize <= 0:
		return fmt.Errorf("segment size must be positive, got %d", s.SegmentSize)
	case s.SegmentSize > TelegramFileLimit:
		return fmt.Errorf(
			"segment size %d exceeds the %d byte ceiling of one Telegram object (%d parts of %d bytes)",
			s.SegmentSize, TelegramFileLimit, MaxUploadParts, UploadPartSize)
	case s.SegmentSize%UploadPartSize != 0:
		return fmt.Errorf("segment size %d must be a multiple of the %d byte upload part size",
			s.SegmentSize, UploadPartSize)
	case s.PoolSize < 1:
		return fmt.Errorf("telegram pool size must be at least 1, got %d", s.PoolSize)
	case s.UploadThreads < 1:
		return fmt.Errorf("upload threads must be at least 1, got %d", s.UploadThreads)
	case s.StreamConcurrency < 1:
		return fmt.Errorf("stream concurrency must be at least 1, got %d", s.StreamConcurrency)
	case s.LogLevel != "" && !validLogLevel(s.LogLevel):
		return fmt.Errorf("invalid log level %q", s.LogLevel)
	}
	return nil
}

func validLogLevel(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "info", "warn", "error", "dpanic", "panic", "fatal":
		return true
	default:
		return false
	}
}

// Runtime setting keys are shared with the database settings table. They are
// kept here too so startup loading and API validation use one vocabulary.
const (
	SettingTGAppID           = "telegram.app_id"
	SettingTGAppHash         = "telegram.app_hash"
	SettingSegmentSize       = "storage.segment_size"
	SettingTGPoolSize        = "telegram.pool_size"
	SettingUploadThreads     = "telegram.upload_threads"
	SettingStreamConcurrency = "stream.concurrency"
	SettingWebDAVEnabled     = "webdav.enabled"
	SettingLogLevel          = "log.level"
)

// RuntimeSettingStore is the small part of the database API needed at startup.
type RuntimeSettingStore interface {
	SettingOr(ctx context.Context, key, def string) string
}

// ApplyStoredRuntimeSettings loads settings saved by the WebUI. Existing
// environment values are used as the initial fallback, so older deployments
// continue to work and a saved WebUI value takes precedence over them.
func (c *Config) ApplyStoredRuntimeSettings(ctx context.Context, store RuntimeSettingStore) error {
	s := c.RuntimeSettings()

	if value := store.SettingOr(ctx, SettingTGAppID, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored telegram app id %q is not a number: %w", value, err)
		}
		s.AppID = n
	}
	if value := store.SettingOr(ctx, SettingTGAppHash, ""); value != "" {
		s.AppHash = value
	}
	if value := store.SettingOr(ctx, SettingSegmentSize, ""); value != "" {
		n, err := ParseSize(value)
		if err != nil {
			return fmt.Errorf("stored segment size %q is invalid: %w", value, err)
		}
		s.SegmentSize = n
	}
	if value := store.SettingOr(ctx, SettingTGPoolSize, ""); value != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return fmt.Errorf("stored telegram pool size %q is invalid: %w", value, err)
		}
		s.PoolSize = n
	}
	if value := store.SettingOr(ctx, SettingUploadThreads, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored upload threads %q is invalid: %w", value, err)
		}
		s.UploadThreads = n
	}
	if value := store.SettingOr(ctx, SettingStreamConcurrency, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored stream concurrency %q is invalid: %w", value, err)
		}
		s.StreamConcurrency = n
	}
	if value := store.SettingOr(ctx, SettingWebDAVEnabled, ""); value != "" {
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored WebDAV setting %q is invalid: %w", value, err)
		}
		s.WebDAVEnabled = enabled
	}
	if value := store.SettingOr(ctx, SettingLogLevel, ""); value != "" {
		s.LogLevel = strings.ToLower(strings.TrimSpace(value))
	}

	if err := s.Validate(); err != nil {
		return fmt.Errorf("stored runtime settings: %w", err)
	}
	c.SetRuntimeSettings(s)
	return nil
}
