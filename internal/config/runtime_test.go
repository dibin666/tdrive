package config

import (
	"context"
	"testing"
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
	if got.StreamConcurrency != 2 || got.WebDAVEnabled || got.LogLevel != "debug" {
		t.Fatalf("runtime values = %+v", got)
	}
}

func TestRuntimeSettingsRejectInvalidValues(t *testing.T) {
	settings := RuntimeSettings{
		SegmentSize:       DefaultSegmentSize,
		PoolSize:          8,
		UploadThreads:     8,
		StreamConcurrency: 6,
		LogLevel:          "info",
	}

	settings.SegmentSize++
	if err := settings.Validate(); err == nil {
		t.Fatal("accepted a segment size that is not an upload-part multiple")
	}
}
