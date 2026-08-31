package plugin

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// Update checking.
//
// Nothing here installs anything. A newer release is only ever reported, and
// installing it goes back through Inspect and Install exactly as a fresh
// installation does — the administrator reviews the same manifest, the same
// binary digest and the same warning. An update is not a reason to trust new
// bytes less than the first time, so it does not get a shortcut around the
// review either.

// updateCheckTTL keeps the settings page from re-fetching every manifest each
// time somebody opens it. The answer changes when a plugin author publishes a
// release, which is not something worth a network round trip per page view; the
// refresh button bypasses this for the person who wants to know right now.
const updateCheckTTL = 15 * time.Minute

// PluginUpdate is what an installed plugin's update state looks like to the
// WebUI.
type PluginUpdate struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion,omitempty"`
	// ManifestURL and ManifestDigest feed straight back into Inspect, so the
	// update installs through the review-and-confirm flow rather than beside it.
	ManifestURL    string `json:"manifestUrl,omitempty"`
	ManifestDigest string `json:"manifestDigest,omitempty"`
	Available      bool   `json:"available"`
	// Origin says where the answer came from: "store" for the configured index,
	// "manifest" for the URL the plugin was installed from. They differ in how
	// much they can tell you — a manifest URL that pins a release will always
	// report the version already installed.
	Origin string `json:"origin,omitempty"`
	// Error explains why this one plugin could not be checked. One unreachable
	// manifest must not turn the whole check into a failure, because the other
	// plugins' answers are still worth having.
	Error string `json:"error,omitempty"`
}

// UpdateReport is one whole check, cached until it goes stale.
type UpdateReport struct {
	Plugins   []PluginUpdate `json:"plugins"`
	CheckedAt time.Time      `json:"checkedAt"`
	// Available is the number of plugins with a newer release, so the settings
	// navigation can carry a badge without walking the list.
	Available int `json:"available"`
	// StoreError records a store index that could not be read. The check still
	// succeeds — every plugin falls back to its own manifest URL — but saying so
	// is better than silently reporting no updates.
	StoreError string `json:"storeError,omitempty"`
}

type updateCache struct {
	mu     sync.Mutex
	report UpdateReport
	fresh  bool
}

// CheckUpdates reports which installed plugins have a newer release.
//
// force skips the cache. Without it, a report younger than updateCheckTTL is
// returned as it stands.
func (manager *Manager) CheckUpdates(ctx context.Context, force bool) (UpdateReport, error) {
	if !force {
		if cached, ok := manager.cachedUpdates(); ok {
			return cached, nil
		}
	}

	records, err := manager.db.ListPlugins(ctx)
	if err != nil {
		return UpdateReport{}, err
	}
	report := UpdateReport{Plugins: make([]PluginUpdate, 0, len(records)), CheckedAt: time.Now()}
	if len(records) == 0 {
		manager.storeUpdates(report)
		return report, nil
	}

	// The store index answers for every plugin it lists, so it is fetched once
	// rather than once per row. A store that is not configured returns an empty
	// index and no error, which is exactly the right behaviour here: each plugin
	// then falls back to the manifest URL it was installed from.
	store := make(map[string]StorePlugin)
	if index, err := manager.Store(ctx, ""); err != nil {
		report.StoreError = err.Error()
		manager.log.Debug("could not read the plugin store while checking for updates", zap.Error(err))
	} else {
		for _, item := range index.Plugins {
			store[item.ID] = item
		}
	}

	manager.mu.RLock()
	fetcher := manager.fetcher
	manager.mu.RUnlock()

	for _, record := range records {
		update := PluginUpdate{
			ID:             record.ID,
			Name:           record.Name,
			CurrentVersion: record.Version,
		}
		if item, ok := store[record.ID]; ok {
			update.Origin = "store"
			update.LatestVersion = item.Version
			update.ManifestURL = item.ManifestURL
			update.ManifestDigest = item.ManifestDigest
		} else if record.ManifestURL != "" {
			// A manifest URL that pins a release answers with the version that
			// is already installed, which is a correct "up to date" rather than
			// a failure. One that tracks a branch answers with the new version.
			update.Origin = "manifest"
			update.ManifestURL = record.ManifestURL
			switch {
			case fetcher == nil:
				update.Error = "plugin installer is not configured"
			default:
				manifest, digest, err := fetcher.Manifest(ctx, record.ManifestURL)
				if err != nil {
					update.Error = err.Error()
				} else {
					update.LatestVersion = manifest.Version
					update.ManifestDigest = digest
				}
			}
		} else {
			update.Error = "这个插件没有记录安装来源，无法检查更新"
		}

		if update.LatestVersion != "" {
			update.Available = tdriveplugin.IsNewerVersion(update.LatestVersion, record.Version)
		}
		if update.Available {
			report.Available++
		}
		report.Plugins = append(report.Plugins, update)
	}

	manager.storeUpdates(report)
	return report, nil
}

func (manager *Manager) cachedUpdates() (UpdateReport, bool) {
	manager.updates.mu.Lock()
	defer manager.updates.mu.Unlock()
	if !manager.updates.fresh || time.Since(manager.updates.report.CheckedAt) > updateCheckTTL {
		return UpdateReport{}, false
	}
	return manager.updates.report, true
}

func (manager *Manager) storeUpdates(report UpdateReport) {
	manager.updates.mu.Lock()
	manager.updates.report = report
	manager.updates.fresh = true
	manager.updates.mu.Unlock()
}

// invalidateUpdates drops the cached report after anything that changes what it
// would say: an installation, an uninstallation.
func (manager *Manager) invalidateUpdates() {
	manager.updates.mu.Lock()
	manager.updates.fresh = false
	manager.updates.mu.Unlock()
}
