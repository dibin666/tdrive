package database

import (
	"context"
	"fmt"
)

const pluginColumns = `user_id, id, name, version, author, enabled, status, source,
	manifest_url, manifest_digest, binary_digest, binary_path, manifest_json,
	error, installed_at, updated_at`

func scanPlugin(row interface{ Scan(...any) error }) (PluginRecord, error) {
	var (
		pluginRecord PluginRecord
		enabled      int
		installedAt  int64
		updatedAt    int64
	)
	if err := row.Scan(
		&pluginRecord.UserID,
		&pluginRecord.ID,
		&pluginRecord.Name,
		&pluginRecord.Version,
		&pluginRecord.Author,
		&enabled,
		&pluginRecord.Status,
		&pluginRecord.Source,
		&pluginRecord.ManifestURL,
		&pluginRecord.ManifestDigest,
		&pluginRecord.BinaryDigest,
		&pluginRecord.BinaryPath,
		&pluginRecord.ManifestJSON,
		&pluginRecord.Error,
		&installedAt,
		&updatedAt,
	); err != nil {
		return PluginRecord{}, Translate(err)
	}
	pluginRecord.Enabled = enabled != 0
	pluginRecord.InstalledAt = msToTime(installedAt)
	pluginRecord.UpdatedAt = msToTime(updatedAt)
	return pluginRecord, nil
}

func scanPlugins(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]PluginRecord, error) {
	defer rows.Close()

	var plugins []PluginRecord
	for rows.Next() {
		pluginRecord, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, pluginRecord)
	}
	return plugins, Translate(rows.Err())
}

// ListPlugins returns one account's installation metadata in a stable display
// order. A plugin belongs to whoever installed it, so this is the whole of
// what that account is allowed to see.
func (d *DB) ListPlugins(ctx context.Context, userID string) ([]PluginRecord, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+pluginColumns+` FROM plugins WHERE user_id = ? ORDER BY name, id`, userID)
	if err != nil {
		return nil, Translate(err)
	}
	return scanPlugins(rows)
}

// ListAllEnabledPlugins is the only plugin query used during startup: every
// account's enabled plugins, because the manager has to bring all of them up.
// It is deliberately not called ListEnabledPlugins any more — the meaning
// changed from "this deployment's plugins" to "everybody's plugins" without
// the signature changing, and a rename is what forces the one call site to be
// looked at. The order is deterministic so a process cap admits the same set
// across restarts.
func (d *DB) ListAllEnabledPlugins(ctx context.Context) ([]PluginRecord, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+pluginColumns+` FROM plugins WHERE enabled = 1 ORDER BY user_id, name, id`)
	if err != nil {
		return nil, Translate(err)
	}
	return scanPlugins(rows)
}

// PluginByID loads one account's installation of a plugin.
func (d *DB) PluginByID(ctx context.Context, userID, id string) (PluginRecord, error) {
	return scanPlugin(d.read.QueryRowContext(ctx,
		`SELECT `+pluginColumns+` FROM plugins WHERE user_id = ? AND id = ?`, userID, id))
}

// CountPluginsForUser backs the per-account installation cap.
func (d *DB) CountPluginsForUser(ctx context.Context, userID string) (int, error) {
	var count int
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plugins WHERE user_id = ?`, userID).Scan(&count)
	return count, Translate(err)
}

// CountEnabledPlugins backs the deployment-wide child-process cap.
func (d *DB) CountEnabledPlugins(ctx context.Context) (int, error) {
	var count int
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM plugins WHERE enabled = 1`).Scan(&count)
	return count, Translate(err)
}

// UpsertPlugin persists a fully validated installation atomically from the
// database's point of view. The manager performs the filesystem and process
// transaction around this call.
func (d *DB) UpsertPlugin(ctx context.Context, pluginRecord PluginRecord) error {
	_, err := d.write.ExecContext(ctx, `
		INSERT INTO plugins (
			user_id, id, name, version, author, enabled, status, source, manifest_url,
			manifest_digest, binary_digest, binary_path, manifest_json, error,
			installed_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, id) DO UPDATE SET
			name = excluded.name,
			version = excluded.version,
			author = excluded.author,
			enabled = excluded.enabled,
			status = excluded.status,
			source = excluded.source,
			manifest_url = excluded.manifest_url,
			manifest_digest = excluded.manifest_digest,
			binary_digest = excluded.binary_digest,
			binary_path = excluded.binary_path,
			manifest_json = excluded.manifest_json,
			error = excluded.error,
			updated_at = excluded.updated_at`,
		pluginRecord.UserID,
		pluginRecord.ID,
		pluginRecord.Name,
		pluginRecord.Version,
		pluginRecord.Author,
		boolInt(pluginRecord.Enabled),
		pluginRecord.Status,
		pluginRecord.Source,
		pluginRecord.ManifestURL,
		pluginRecord.ManifestDigest,
		pluginRecord.BinaryDigest,
		pluginRecord.BinaryPath,
		pluginRecord.ManifestJSON,
		pluginRecord.Error,
		pluginRecord.InstalledAt.UnixMilli(),
		pluginRecord.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("upsert plugin %q: %w", pluginRecord.ID, Translate(err))
	}
	return nil
}

// UpdatePluginState changes only lifecycle fields and returns the new record.
func (d *DB) UpdatePluginState(ctx context.Context, userID, id string, enabled bool, status, message string) (PluginRecord, error) {
	result, err := d.write.ExecContext(ctx, `
		UPDATE plugins SET enabled = ?, status = ?, error = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, boolInt(enabled), status, message, nowMS(), userID, id)
	if err := affectedOne(result, err, "update plugin state"); err != nil {
		return PluginRecord{}, err
	}
	return d.PluginByID(ctx, userID, id)
}

// UpdatePluginStatus changes only lifecycle information. In particular, it
// never changes enabled: a runtime failure must not turn the owner's
// concurrent disable into an enabled plugin again.
func (d *DB) UpdatePluginStatus(ctx context.Context, userID, id, status, message string) (PluginRecord, error) {
	result, err := d.write.ExecContext(ctx, `
		UPDATE plugins SET status = ?, error = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, status, message, nowMS(), userID, id)
	if err := affectedOne(result, err, "update plugin status"); err != nil {
		return PluginRecord{}, err
	}
	return d.PluginByID(ctx, userID, id)
}

// UpdatePluginStatusIfEnabled is the recovery counterpart to
// UpdatePluginStatus. A plugin that was disabled or uninstalled while its
// replacement was starting must not be made active by the stale recovery
// goroutine.
func (d *DB) UpdatePluginStatusIfEnabled(ctx context.Context, userID, id, status, message string) (PluginRecord, error) {
	result, err := d.write.ExecContext(ctx, `
		UPDATE plugins SET status = ?, error = ?, updated_at = ?
		WHERE user_id = ? AND id = ? AND enabled = 1`, status, message, nowMS(), userID, id)
	if err := affectedOne(result, err, "update enabled plugin status"); err != nil {
		return PluginRecord{}, err
	}
	return d.PluginByID(ctx, userID, id)
}

// DeletePlugin removes metadata and namespaced state after the manager has
// stopped the child process and removed its binary.
func (d *DB) DeletePlugin(ctx context.Context, userID, id string) error {
	result, err := d.write.ExecContext(ctx,
		`DELETE FROM plugins WHERE user_id = ? AND id = ?`, userID, id)
	return affectedOne(result, err, "delete plugin")
}

// PluginData reads a small namespaced value owned by one account's plugin.
func (d *DB) PluginData(ctx context.Context, userID, pluginID, key string) ([]byte, error) {
	var value []byte
	err := d.read.QueryRowContext(ctx,
		`SELECT value FROM plugin_data WHERE user_id = ? AND plugin_id = ? AND key = ?`,
		userID, pluginID, key).Scan(&value)
	return value, Translate(err)
}

// SetPluginData writes a small namespaced value owned by one account's plugin.
func (d *DB) SetPluginData(ctx context.Context, userID, pluginID, key string, value []byte) error {
	_, err := d.write.ExecContext(ctx, `
		INSERT INTO plugin_data (user_id, plugin_id, key, value, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, plugin_id, key) DO UPDATE SET
			value = excluded.value, updated_at = excluded.updated_at`,
		userID, pluginID, key, value, nowMS())
	return Translate(err)
}

func (d *DB) DeletePluginData(ctx context.Context, userID, pluginID, key string) error {
	_, err := d.write.ExecContext(ctx,
		`DELETE FROM plugin_data WHERE user_id = ? AND plugin_id = ? AND key = ?`,
		userID, pluginID, key)
	return Translate(err)
}
