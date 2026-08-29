package config

import (
	"context"
	"testing"
	"time"
)

type testRuntimeStore map[string]string

func (s testRuntimeStore) SettingOr(_ context.Context, key, def string) string {
	if value, ok := s[key]; ok {
		return value
	}
	return def
}

func TestApplyStoredRuntimeSettings(t *testing.T) {
	cfg := &Config{
		Telegram: Telegram{PoolSize: 8, UploadThreads: 8},
		Storage:  Storage{SegmentSize: DefaultSegmentSize, SegmentConcurrency: 2},
		Stream:   Stream{Concurrency: 6, Buffers: 8},
		WebDAV:   WebDAV{Enabled: true},
		LogLevel: "info",
	}

	err := cfg.ApplyStoredRuntimeSettings(context.Background(), testRuntimeStore{
		SettingTGAppID:           "12345",
		SettingTGAppHash:         "hash",
		SettingSegmentSize:       "1000MiB",
		SettingTGPoolSize:        "4",
		SettingUploadThreads:     "3",
		SettingTGUploadPartSize:  "256KiB",
		SettingTGRateLimit:       "50ms",
		SettingStreamConcurrency: "2",
		SettingWebDAVEnabled:     "false",
		SettingLogLevel:          "DEBUG",
	})
	if err != nil {
		t.Fatalf("apply stored settings: %v", err)
	}

	got := cfg.RuntimeSettings()
	if got.AppID != 12345 || got.AppHash != "hash" {
		t.Fatalf("telegram credentials = %d/%q", got.AppID, got.AppHash)
	}
	if got.SegmentSize != 1000*1024*1024 || got.PoolSize != 4 || got.UploadThreads != 3 {
		t.Fatalf("runtime values = %+v", got)
	}
	if got.UploadPartSize != 256*1024 || got.RateLimit != 50*time.Millisecond {
		t.Fatalf("telegram upload values = %+v", got)
	}
	if got.StreamConcurrency != 2 || got.WebDAVEnabled || got.LogLevel != "debug" {
		t.Fatalf("runtime values = %+v", got)
	}
}

func TestRuntimeSettingsRejectInvalidValues(t *testing.T) {
	settings := RuntimeSettings{
		SegmentSize:       DefaultSegmentSize,
		PoolSize:          8,
		UploadThreads:     8,
		UploadPartSize:    DefaultUploadPartSize,
		RateLimit:         DefaultRateLimit,
		StreamConcurrency: 6,
		LogLevel:          "info",
	}

	settings.SegmentSize++
	if err := settings.Validate(); err == nil {
		t.Fatal("accepted a segment size that is not an upload-part multiple")
	}
}

func TestRuntimeSettingsValidateTelegramUploadValues(t *testing.T) {
	base := RuntimeSettings{
		SegmentSize:       DefaultSegmentSize,
		PoolSize:          8,
		UploadThreads:     8,
		UploadPartSize:    DefaultUploadPartSize,
		RateLimit:         DefaultRateLimit,
		StreamConcurrency: 6,
		LogLevel:          "info",
	}

	smaller := base
	smaller.UploadPartSize = 256 * 1024
	smaller.SegmentSize = 1000 * 1024 * 1024
	if err := smaller.Validate(); err != nil {
		t.Fatalf("accepted a valid smaller upload part size: %v", err)
	}

	badPart := base
	badPart.UploadPartSize = 100 * 1024
	if err := badPart.Validate(); err == nil {
		t.Fatal("accepted an upload part size that does not divide 512 KiB")
	}

	tooSlow := base
	tooSlow.RateLimit = 0
	if err := tooSlow.Validate(); err == nil {
		t.Fatal("accepted a non-positive request interval")
	}
}
