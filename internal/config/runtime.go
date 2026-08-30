package config

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RuntimeSettings are the settings that may be changed from the WebUI. The
// deployment settings (data directory, listen address, public URL and the
// first administrator) remain environment-only because changing them would
// require moving files, rebinding the server or changing the current account.
type RuntimeSettings struct {
	AppID   int
	AppHash string
	// LocalRoot is the absolute directory exposed by the VPS-local upload
	// picker. It may point at a Docker bind mount and can be changed without a
	// restart.
	LocalRoot         string
	SegmentSize       int64
	PoolSize          int64
	UploadThreads     int
	UploadPartSize    int64
	RateLimit         time.Duration
	StreamConcurrency int
	// UploadConcurrency and DownloadConcurrency limit whole-file tasks. The
	// Telegram part/chunk settings above remain per-task limits.
	UploadConcurrency   int
	DownloadConcurrency int
	WebDAVEnabled       bool
	LogLevel            string

	// CacheDir is where staged downloads are assembled. Empty means the
	// default under the data directory.
	CacheDir string
	// CacheLimit bounds the disk staged downloads may hold; 0 disables
	// staging entirely, which is how an operator with no spare disk turns the
	// feature off.
	CacheLimit int64
	// CacheTTL is how long a staged copy survives after it is ready.
	CacheTTL time.Duration
	// MaxDownloadConns caps the parallel range requests one logical download
	// may hold at once.
	MaxDownloadConns int
	// DownloadGrace is how long a download session keeps its task slot after
	// its last request finishes, so a parallel downloader does not lose its
	// place between ranges.
	DownloadGrace time.Duration
	// ShareTTL is the default lifetime of a new share link; 0 means links
	// default to never expiring.
	ShareTTL time.Duration
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
		return withRuntimeDefaults(c.Runtime.Snapshot())
	}
	return withRuntimeDefaults(RuntimeSettings{
		AppID:               c.Telegram.AppID,
		AppHash:             c.Telegram.AppHash,
		LocalRoot:           c.Local.Root,
		SegmentSize:         c.Storage.SegmentSize,
		PoolSize:            c.Telegram.PoolSize,
		UploadThreads:       c.Telegram.UploadThreads,
		UploadPartSize:      c.Telegram.UploadPartSize,
		RateLimit:           c.Telegram.RateLimit,
		StreamConcurrency:   c.Stream.Concurrency,
		UploadConcurrency:   c.Transfer.UploadConcurrency,
		DownloadConcurrency: c.Transfer.DownloadConcurrency,
		WebDAVEnabled:       c.WebDAV.Enabled,
		LogLevel:            c.LogLevel,
		CacheDir:            c.Storage.CacheDir,
		CacheLimit:          c.Storage.CacheLimit,
		CacheTTL:            c.Storage.CacheTTL,
		MaxDownloadConns:    c.Transfer.MaxDownloadConns,
		DownloadGrace:       c.Transfer.DownloadGrace,
		ShareTTL:            c.Transfer.ShareTTL,
	})
}

// CacheRoot is the directory staged downloads live in, resolved against the
// deployment default when the runtime value is blank.
func (c *Config) CacheRoot() string {
	if dir := c.RuntimeSettings().CacheDir; dir != "" {
		return dir
	}
	return c.Storage.CacheDir
}

func withRuntimeDefaults(settings RuntimeSettings) RuntimeSettings {
	if settings.UploadPartSize <= 0 {
		settings.UploadPartSize = DefaultUploadPartSize
	}
	if settings.RateLimit <= 0 {
		settings.RateLimit = DefaultRateLimit
	}
	if settings.UploadConcurrency <= 0 {
		settings.UploadConcurrency = DefaultUploadConcurrency
	}
	if settings.DownloadConcurrency <= 0 {
		settings.DownloadConcurrency = DefaultDownloadConcurrency
	}
	if settings.CacheTTL <= 0 {
		settings.CacheTTL = DefaultCacheTTL
	}
	if settings.MaxDownloadConns <= 0 {
		settings.MaxDownloadConns = DefaultMaxDownloadConns
	}
	if settings.DownloadGrace <= 0 {
		settings.DownloadGrace = DefaultDownloadGrace
	}
	// CacheLimit and ShareTTL are deliberately absent here: zero is a
	// meaningful value for both ("staging disabled", "links never expire"),
	// so a default can only be applied where the setting is first created.
	return settings
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
//
// The fields that predate withRuntimeDefaults are checked strictly, including
// rejecting zero: a caller that reaches Validate with, say, no request
// interval has a bug. The fields added later treat zero as "not set" and let
// withRuntimeDefaults fill them in, so a client that does not know about a
// newer field cannot be told its otherwise-valid settings are invalid.
func (s RuntimeSettings) Validate() error {
	switch {
	case s.AppID < 0:
		return fmt.Errorf("telegram app id must not be negative")
	case s.UploadPartSize <= 0:
		return fmt.Errorf("telegram upload part size must be positive, got %d", s.UploadPartSize)
	case s.UploadPartSize%1024 != 0:
		return fmt.Errorf("telegram upload part size %d must be a multiple of 1024 bytes", s.UploadPartSize)
	case s.UploadPartSize > DefaultUploadPartSize:
		return fmt.Errorf("telegram upload part size %d exceeds the %d byte maximum", s.UploadPartSize, DefaultUploadPartSize)
	case DefaultUploadPartSize%s.UploadPartSize != 0:
		return fmt.Errorf("telegram upload part size %d must divide the %d byte maximum", s.UploadPartSize, DefaultUploadPartSize)
	case s.RateLimit < time.Millisecond:
		return fmt.Errorf("telegram request interval must be at least 1ms, got %s", s.RateLimit)
	case s.RateLimit > time.Minute:
		return fmt.Errorf("telegram request interval must not exceed 1m, got %s", s.RateLimit)
	case s.SegmentSize <= 0:
		return fmt.Errorf("segment size must be positive, got %d", s.SegmentSize)
	case s.SegmentSize > MaxSegmentSize(s.UploadPartSize):
		return fmt.Errorf(
			"segment size %d exceeds the %d byte ceiling of one Telegram object (%d parts of %d bytes)",
			s.SegmentSize, MaxSegmentSize(s.UploadPartSize), MaxUploadParts, s.UploadPartSize)
	case s.SegmentSize%s.UploadPartSize != 0:
		return fmt.Errorf("segment size %d must be a multiple of the %d byte Telegram upload part size",
			s.SegmentSize, s.UploadPartSize)
	case s.PoolSize < 1:
		return fmt.Errorf("telegram pool size must be at least 1, got %d", s.PoolSize)
	case s.UploadThreads < 1:
		return fmt.Errorf("upload threads must be at least 1, got %d", s.UploadThreads)
	case s.StreamConcurrency < 1:
		return fmt.Errorf("stream concurrency must be at least 1, got %d", s.StreamConcurrency)
	case s.UploadConcurrency < 1:
		return fmt.Errorf("upload task concurrency must be at least 1, got %d", s.UploadConcurrency)
	case s.DownloadConcurrency < 1:
		return fmt.Errorf("download task concurrency must be at least 1, got %d", s.DownloadConcurrency)
	case s.MaxDownloadConns < 0 || s.MaxDownloadConns > 64:
		return fmt.Errorf("download connections per transfer must be between 1 and 64, got %d", s.MaxDownloadConns)
	case s.CacheLimit < 0:
		return fmt.Errorf("download cache limit must not be negative, got %d", s.CacheLimit)
	case s.CacheTTL < 0 || (s.CacheTTL > 0 && s.CacheTTL < time.Minute):
		return fmt.Errorf("download cache lifetime must be at least 1m, got %s", s.CacheTTL)
	case s.DownloadGrace < 0 || s.DownloadGrace > 5*time.Minute:
		return fmt.Errorf("download session grace must not exceed 5m, got %s", s.DownloadGrace)
	case s.ShareTTL < 0:
		return fmt.Errorf("share link lifetime must not be negative, got %s", s.ShareTTL)
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
	SettingTGAppID             = "telegram.app_id"
	SettingTGAppHash           = "telegram.app_hash"
	SettingLocalRoot           = "local.root"
	SettingSegmentSize         = "storage.segment_size"
	SettingTGPoolSize          = "telegram.pool_size"
	SettingUploadThreads       = "telegram.upload_threads"
	SettingTGUploadPartSize    = "telegram.upload_part_size"
	SettingTGRateLimit         = "telegram.rate_limit"
	SettingStreamConcurrency   = "stream.concurrency"
	SettingUploadConcurrency   = "transfer.upload_concurrency"
	SettingDownloadConcurrency = "transfer.download_concurrency"
	SettingWebDAVEnabled       = "webdav.enabled"
	SettingLogLevel            = "log.level"
	SettingCacheDir            = "download.cache_dir"
	SettingCacheLimit          = "download.cache_limit"
	SettingCacheTTL            = "download.cache_ttl"
	SettingMaxDownloadConns    = "download.max_conns"
	SettingDownloadGrace       = "download.session_grace"
	SettingShareTTL            = "share.default_ttl"
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
	// A stored empty string is meaningful: it explicitly disables local
	// uploads, even if a legacy TDRIVE_LOCAL_DIR environment fallback exists.
	const missingSetting = "\x00"

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
	if value := store.SettingOr(ctx, SettingLocalRoot, missingSetting); value != missingSetting {
		localRoot, err := NormalizeLocalRoot(value)
		if err != nil {
			return fmt.Errorf("stored local directory %q is invalid: %w", value, err)
		}
		s.LocalRoot = localRoot
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
	if value := store.SettingOr(ctx, SettingTGUploadPartSize, ""); value != "" {
		n, err := ParseSize(value)
		if err != nil {
			return fmt.Errorf("stored telegram upload part size %q is invalid: %w", value, err)
		}
		s.UploadPartSize = n
	}
	if value := store.SettingOr(ctx, SettingTGRateLimit, ""); value != "" {
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored telegram request interval %q is invalid: %w", value, err)
		}
		s.RateLimit = d
	}
	if value := store.SettingOr(ctx, SettingStreamConcurrency, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored stream concurrency %q is invalid: %w", value, err)
		}
		s.StreamConcurrency = n
	}
	if value := store.SettingOr(ctx, SettingUploadConcurrency, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored upload task concurrency %q is invalid: %w", value, err)
		}
		s.UploadConcurrency = n
	}
	if value := store.SettingOr(ctx, SettingDownloadConcurrency, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored download task concurrency %q is invalid: %w", value, err)
		}
		s.DownloadConcurrency = n
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
	// The cache directory follows the same rule as the local upload root: a
	// stored empty string is a deliberate "use the default", not a missing
	// setting, so it is read with the sentinel too.
	if value := store.SettingOr(ctx, SettingCacheDir, missingSetting); value != missingSetting {
		s.CacheDir = strings.TrimSpace(value)
	}
	if value := store.SettingOr(ctx, SettingCacheLimit, ""); value != "" {
		n, err := ParseSize(value)
		if err != nil {
			return fmt.Errorf("stored download cache limit %q is invalid: %w", value, err)
		}
		s.CacheLimit = n
	}
	if value := store.SettingOr(ctx, SettingCacheTTL, ""); value != "" {
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored download cache lifetime %q is invalid: %w", value, err)
		}
		s.CacheTTL = d
	}
	if value := store.SettingOr(ctx, SettingMaxDownloadConns, ""); value != "" {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored download connection limit %q is invalid: %w", value, err)
		}
		s.MaxDownloadConns = n
	}
	if value := store.SettingOr(ctx, SettingDownloadGrace, ""); value != "" {
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored download session grace %q is invalid: %w", value, err)
		}
		s.DownloadGrace = d
	}
	if value := store.SettingOr(ctx, SettingShareTTL, ""); value != "" {
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("stored share link lifetime %q is invalid: %w", value, err)
		}
		s.ShareTTL = d
	}

	if err := s.Validate(); err != nil {
		return fmt.Errorf("stored runtime settings: %w", err)
	}
	c.SetRuntimeSettings(s)
	return nil
}

// MaxSegmentSize is the largest logical segment that can fit into one
// Telegram object for the selected upload part size.
func MaxSegmentSize(partSize int64) int64 {
	return int64(MaxUploadParts) * partSize
}
