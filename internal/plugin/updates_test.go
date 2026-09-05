package plugin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

func newUpdateManager(t *testing.T, latest string) (*Manager, *database.DB, *fakeReleaseFetcher, database.User) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	fetcher := &fakeReleaseFetcher{
		manifest: tdriveplugin.Manifest{
			ID: "example", Name: "Example", Version: latest,
			SDKVersion: "0.1", APIVersion: tdriveplugin.APIVersion,
			Author: "Example", License: "MIT",
			RepositoryURL: "https://example.com/plugin",
		},
		digest: "manifest-digest",
	}
	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	manager.SetFetcher(fetcher)
	return manager, db, fetcher, newTestUser(t, db, "owner")
}

func installRecord(t *testing.T, db *database.DB, owner database.User, version string) {
	t.Helper()
	now := time.Now()
	err := db.UpsertPlugin(context.Background(), database.PluginRecord{
		UserID: owner.ID, ID: "example", Name: "Example", Version: version, Author: "Example",
		Enabled: true, Status: database.PluginStatusActive, Source: "release",
		ManifestURL: "https://example.com/plugin/tdrive.plugin.json",
		InstalledAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertPlugin: %v", err)
	}
}

// Without this check there is no way to learn that a plugin has a new release
// short of visiting its repository, which is the complaint that motivated it.
func TestCheckUpdatesReportsANewerRelease(t *testing.T) {
	ctx := context.Background()
	manager, db, _, owner := newUpdateManager(t, "1.2.0")
	installRecord(t, db, owner, "1.1.0")

	report, err := manager.CheckUpdates(ctx, owner.ID, true)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if report.Available != 1 || len(report.Plugins) != 1 {
		t.Fatalf("report = %+v, want one available update", report)
	}
	update := report.Plugins[0]
	if !update.Available || update.LatestVersion != "1.2.0" || update.CurrentVersion != "1.1.0" {
		t.Fatalf("update = %+v", update)
	}
	// The install flow takes it from here, so it has to carry what Inspect needs
	// to pin the same manifest the check just read.
	if update.ManifestURL == "" || update.ManifestDigest == "" {
		t.Errorf("update carries no manifest to install from: %+v", update)
	}
}

// A manifest URL that pins a release keeps answering with the installed
// version. That is "up to date", not a problem to report.
func TestCheckUpdatesReportsNothingWhenTheVersionMatches(t *testing.T) {
	ctx := context.Background()
	manager, db, _, owner := newUpdateManager(t, "1.1.0")
	installRecord(t, db, owner, "1.1.0")

	report, err := manager.CheckUpdates(ctx, owner.ID, true)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if report.Available != 0 {
		t.Fatalf("report = %+v, want no available updates", report)
	}
	if report.Plugins[0].Error != "" {
		t.Errorf("unexpected error for an up-to-date plugin: %q", report.Plugins[0].Error)
	}
}

// Opening the settings page must not re-fetch every plugin manifest, and the
// refresh button must be able to.
func TestCheckUpdatesCachesUntilForced(t *testing.T) {
	ctx := context.Background()
	manager, db, fetcher, owner := newUpdateManager(t, "2.0.0")
	installRecord(t, db, owner, "1.0.0")

	if _, err := manager.CheckUpdates(ctx, owner.ID, false); err != nil {
		t.Fatalf("first CheckUpdates: %v", err)
	}
	if _, err := manager.CheckUpdates(ctx, owner.ID, false); err != nil {
		t.Fatalf("second CheckUpdates: %v", err)
	}
	if fetcher.manifestCalls != 1 {
		t.Fatalf("manifest fetched %d times for two checks, want 1", fetcher.manifestCalls)
	}
	if _, err := manager.CheckUpdates(ctx, owner.ID, true); err != nil {
		t.Fatalf("forced CheckUpdates: %v", err)
	}
	if fetcher.manifestCalls != 2 {
		t.Fatalf("manifest fetched %d times, want the forced check to bypass the cache", fetcher.manifestCalls)
	}
}

// A deployment with no plugins should answer instantly and without a request.
func TestCheckUpdatesTouchesNoNetworkWithoutPlugins(t *testing.T) {
	ctx := context.Background()
	manager, _, fetcher, owner := newUpdateManager(t, "1.0.0")

	report, err := manager.CheckUpdates(ctx, owner.ID, true)
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(report.Plugins) != 0 || report.Available != 0 {
		t.Fatalf("report = %+v, want an empty report", report)
	}
	if fetcher.manifestCalls != 0 {
		t.Fatalf("checked %d manifests with nothing installed", fetcher.manifestCalls)
	}
}
