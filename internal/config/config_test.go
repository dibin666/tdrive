package config

import (
	"path/filepath"
	"testing"
)

func TestLoadNormalizesPluginDirToAbsolutePath(t *testing.T) {
	dataDir := t.TempDir()
	pluginDir := filepath.Join(".", "relative-plugin-dir")
	t.Setenv("TDRIVE_DATA_DIR", dataDir)
	t.Setenv("TDRIVE_PLUGIN_DIR", pluginDir)
	t.Setenv("TDRIVE_PLUGIN_STORE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, err := filepath.Abs(pluginDir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if cfg.Plugins.Dir != want {
		t.Fatalf("plugin dir = %q, want %q", cfg.Plugins.Dir, want)
	}
}
