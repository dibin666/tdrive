package database

import (
	"context"
	"fmt"
)

const pluginColumns = `id, name, version, author, enabled, status, source,
	source_url, ref, source_digest, binary_digest, binary_path, manifest_json,
	error, installed_at, updated_at`

func scanPlugin(row interface{ Scan(...any) error }) (PluginRecord, error) {
	var (
		pluginRecord PluginRecord
		enabled      int
		installedAt  int64
		updatedAt    int64
	)
	if err := row.Scan(
		&pluginRecord.ID,
		&pluginRecord.Name,
		&pluginRecord.Version,
		&pluginRecord.Author,
		&enabled,
		&pluginRecord.Status,
		&pluginRecord.Source,
		&pluginRecord.SourceURL,
		&pluginRecord.Ref,
		&pluginRecord.SourceDigest,
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

// ListPlugins returns local installation metadata in a stable display order.
func (d *DB) ListPlugins(ctx context.Context) ([]PluginRecord, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT `+pluginColumns+` FROM plugins ORDER BY name, id`)
	if err != nil {
		return nil, Translate(err)
	}
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

// ListEnabledPlugins is the only plugin query used during startup. It avoids
// opening a source tree or spawning a process for disabled plugins.
func (d *DB) ListEnabledPlugins(ctx context.Context) ([]PluginRecord, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+pluginColumns+` FROM plugins WHERE enabled = 1 ORDER BY name, id`)
	if err != nil {
		return nil, Translate(err)
	}
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

// PluginByID loads one installed plugin.
func (d *DB) PluginByID(ctx context.Context, id string) (PluginRecord, error) {
	return scanPlugin(d.read.QueryRowContext(ctx,
		`SELECT `+pluginColumns+` FROM plugins WHERE id = ?`, id))
}

// UpsertPlugin persists a fully validated installation atomically from the
// database's point of view. The manager performs the filesystem and process
// transaction around this call.
func (d *DB) UpsertPlugin(ctx context.Context, pluginRecord PluginRecord) error {
	_, err := d.write.ExecContext(ctx, `
		INSERT INTO plugins (
			id, name, version, author, enabled, status, source, source_url, ref,
			source_digest, binary_digest, binary_path, manifest_json, error,
			installed_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			version = excluded.version,
			author = excluded.author,
			enabled = excluded.enabled,
			status = excluded.status,
			source = excluded.source,
			source_url = excluded.source_url,
			ref = excluded.ref,
			source_digest = excluded.source_digest,
			binary_digest = excluded.binary_digest,
			binary_path = excluded.binary_path,
			manifest_json = excluded.manifest_json,
			error = excluded.error,
			updated_at = excluded.updated_at`,
		pluginRecord.ID,
		pluginRecord.Name,
		pluginRecord.Version,
		pluginRecord.Author,
		boolInt(pluginRecord.Enabled),
		pluginRecord.Status,
		pluginRecord.Source,
		pluginRecord.SourceURL,
		pluginRecord.Ref,
		pluginRecord.SourceDigest,
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
func (d *DB) UpdatePluginState(ctx context.Context, id string, enabled bool, status, message string) (PluginRecord, error) {
	result, err := d.write.ExecContext(ctx, `
		UPDATE plugins SET enabled = ?, status = ?, error = ?, updated_at = ?
		WHERE id = ?`, boolInt(enabled), status, message, nowMS(), id)
	if err := affectedOne(result, err, "update plugin state"); err != nil {
		return PluginRecord{}, err
	}
	return d.PluginByID(ctx, id)
}

// DeletePlugin removes metadata and namespaced state after the manager has
// stopped the child process and removed its binary.
func (d *DB) DeletePlugin(ctx context.Context, id string) error {
	result, err := d.write.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, id)
	return affectedOne(result, err, "delete plugin")
}

// PluginData reads a small namespaced value owned by one plugin.
func (d *DB) PluginData(ctx context.Context, pluginID, key string) ([]byte, error) {
	var value []byte
	err := d.read.QueryRowContext(ctx,
		`SELECT value FROM plugin_data WHERE plugin_id = ? AND key = ?`, pluginID, key).Scan(&value)
	return value, Translate(err)
}

// SetPluginData writes a small namespaced value owned by one plugin.
func (d *DB) SetPluginData(ctx context.Context, pluginID, key string, value []byte) error {
	_, err := d.write.ExecContext(ctx, `
		INSERT INTO plugin_data (plugin_id, key, value, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (plugin_id, key) DO UPDATE SET
			value = excluded.value, updated_at = excluded.updated_at`,
		pluginID, key, value, nowMS())
	return Translate(err)
}

func (d *DB) DeletePluginData(ctx context.Context, pluginID, key string) error {
	_, err := d.write.ExecContext(ctx,
		`DELETE FROM plugin_data WHERE plugin_id = ? AND key = ?`, pluginID, key)
	return Translate(err)
}
