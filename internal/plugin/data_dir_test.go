package plugin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
)

func TestPluginDataDirUsesPersistentServerDataDir(t *testing.T) {
	dataDir := t.TempDir()
	manager := &Manager{cfg: &config.Config{
		Server:  config.Server{DataDir: dataDir},
		Plugins: config.Plugins{Dir: filepath.Join(t.TempDir(), "ephemeral-plugins")},
	}}

	got := manager.pluginDataDir(database.PluginRecord{UserID: "u1", ID: "aliyunpan"})
	want := filepath.Join(dataDir, "plugin-data", "u1", "aliyunpan")
	if got != want {
		t.Fatalf("pluginDataDir = %q, want %q", got, want)
	}
}

// Two accounts running the same plugin must not share one state directory, or
// one person's credentials become the other person's credentials.
func TestPluginDataDirSeparatesOwners(t *testing.T) {
	manager := &Manager{cfg: &config.Config{Server: config.Server{DataDir: t.TempDir()}}}

	first := manager.pluginDataDir(database.PluginRecord{UserID: "u1", ID: "aliyunpan"})
	second := manager.pluginDataDir(database.PluginRecord{UserID: "u2", ID: "aliyunpan"})
	if first == second {
		t.Fatalf("two accounts share the plugin data directory %q", first)
	}
}

func TestPluginDataDirKeepsLegacyFallbackWhenServerDataDirIsUnset(t *testing.T) {
	pluginDir := t.TempDir()
	manager := &Manager{cfg: &config.Config{Plugins: config.Plugins{Dir: pluginDir}}}

	got := manager.pluginDataDir(database.PluginRecord{UserID: "u1", ID: "aliyunpan"})
	want := filepath.Join(pluginDir, "u1", "aliyunpan-data")
	if got != want {
		t.Fatalf("pluginDataDir = %q, want %q", got, want)
	}
}

func TestPluginEnvironmentReplacesInheritedDataDir(t *testing.T) {
	t.Setenv(pluginDataDirEnv, "/a/stale/path")

	env := pluginEnvironment("/data/plugin-data/aliyunpan")
	var matches []string
	for _, entry := range env {
		if strings.HasPrefix(entry, pluginDataDirEnv+"=") {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("found %d %s entries, want 1: %v", len(matches), pluginDataDirEnv, matches)
	}
	if matches[0] != pluginDataDirEnv+"=/data/plugin-data/aliyunpan" {
		t.Fatalf("data directory environment = %q", matches[0])
	}
}
