package database

import (
	"context"
	"fmt"
)

// Setting keys. These hold values that are chosen at runtime through the setup
// wizard rather than baked into the environment, so that a plain
// `docker run -v tdrive-data:/data` deployment is fully configurable from the
// browser.
const (
	// SettingJWTSecret is generated on first run and persisted so that
	// sessions survive a restart without the operator having to pick a secret.
	SettingJWTSecret = "auth.jwt_secret"
	// SettingTGAppID and SettingTGAppHash hold my.telegram.org credentials
	// entered through the wizard. A stored value takes precedence over the
	// legacy environment fallback once it exists.
	SettingTGAppID   = "telegram.app_id"
	SettingTGAppHash = "telegram.app_hash"
	// SettingSegmentSize records the current default split size. Existing files
	// keep their own segment_size, so changing this only affects new uploads.
	SettingSegmentSize = "storage.segment_size"
	// The remaining runtime settings are editable by an administrator in the
	// WebUI and are applied without restarting the process.
	SettingTGPoolSize        = "telegram.pool_size"
	SettingUploadThreads     = "telegram.upload_threads"
	SettingTGUploadPartSize  = "telegram.upload_part_size"
	SettingTGRateLimit       = "telegram.rate_limit"
	SettingStreamConcurrency = "stream.concurrency"
	SettingWebDAVEnabled     = "webdav.enabled"
	SettingLogLevel          = "log.level"
	// SettingSetupComplete flips once the wizard has run to completion.
	SettingSetupComplete = "setup.complete"
)

func (d *DB) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := d.read.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, Translate(err)
}

// SettingOr returns def when the key is absent, which is the common case for
// optional settings.
func (d *DB) SettingOr(ctx context.Context, key, def string) string {
	v, err := d.Setting(ctx, key)
	if err != nil {
		return def
	}
	return v
}

func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, Translate(err))
	}
	return nil
}

// SetSettings writes a group of related values in one transaction so a failed
// WebUI update cannot leave only half of the runtime configuration persisted.
func (d *DB) SetSettings(ctx context.Context, settings map[string]string) error {
	return d.Tx(ctx, func(tx txExec) error {
		for key, value := range settings {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES (?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
				return fmt.Errorf("set setting %q: %w", key, Translate(err))
			}
		}
		return nil
	})
}

func (d *DB) DeleteSetting(ctx context.Context, key string) error {
	_, err := d.write.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return Translate(err)
}

const channelCols = `id, tg_id, access_hash, title, is_default, created_at`

func scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var (
		c         Channel
		isDefault int
		created   int64
	)
	if err := row.Scan(&c.ID, &c.TGID, &c.AccessHash, &c.Title, &isDefault, &created); err != nil {
		return Channel{}, Translate(err)
	}
	c.IsDefault = isDefault != 0
	c.CreatedAt = msToTime(created)
	return c, nil
}

// UpsertChannel records a storage channel, keyed by its Telegram id so that
// re-selecting the same channel refreshes its access hash instead of
// duplicating it. An access hash can change when the account re-resolves the
// peer, and a stale one makes every download fail.
func (d *DB) UpsertChannel(ctx context.Context, tgID, accessHash int64, title string) (Channel, error) {
	var c Channel
	err := d.Tx(ctx, func(tx txExec) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE tg_id = ?`, tgID).Scan(&id)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx,
				`UPDATE channels SET access_hash = ?, title = ? WHERE id = ?`, accessHash, title, id)
			if err != nil {
				return Translate(err)
			}
		default:
			if e := Translate(err); e != ErrNotFound {
				return e
			}
			id = NewID()
			_, err = tx.ExecContext(ctx,
				`INSERT INTO channels (`+channelCols+`) VALUES (?, ?, ?, ?, 0, ?)`,
				id, tgID, accessHash, title, nowMS())
			if err != nil {
				return Translate(err)
			}
		}
		c, err = scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
		return err
	})
	return c, err
}

func (d *DB) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT `+channelCols+` FROM channels ORDER BY created_at`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, Translate(rows.Err())
}

// DefaultChannel is where new uploads go.
func (d *DB) DefaultChannel(ctx context.Context) (Channel, error) {
	return scanChannel(d.read.QueryRowContext(ctx,
		`SELECT `+channelCols+` FROM channels WHERE is_default = 1 LIMIT 1`))
}

func (d *DB) ChannelByID(ctx context.Context, id string) (Channel, error) {
	return scanChannel(d.read.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
}

// SetDefaultChannel moves the upload target. Existing files keep pointing at
// the channel they were written to, so switching never orphans data.
func (d *DB) SetDefaultChannel(ctx context.Context, id string) error {
	return d.Tx(ctx, func(tx txExec) error {
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET is_default = 0`); err != nil {
			return Translate(err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE channels SET is_default = 1 WHERE id = ?`, id)
		return affectedOne(res, err, "set default channel")
	})
}
