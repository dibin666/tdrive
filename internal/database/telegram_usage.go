package database

import (
	"context"
	"errors"
)

// TelegramUsageFor returns the committed traffic for one account and UTC day.
// A missing row is the normal state for a new day, not an error.
func (d *DB) TelegramUsageFor(ctx context.Context, accountID, date string) (TGTransferUsage, error) {
	usage := TGTransferUsage{AccountID: accountID, Date: date}
	err := d.read.QueryRowContext(ctx,
		`SELECT upload_bytes, download_bytes FROM tg_account_usage
		 WHERE account_id = ? AND quota_date = ?`, accountID, date).
		Scan(&usage.UploadBytes, &usage.DownloadBytes)
	translated := Translate(err)
	if errors.Is(translated, ErrNotFound) {
		return usage, nil
	}
	if translated != nil {
		return TGTransferUsage{}, translated
	}
	return usage, nil
}

// AddTelegramUsage atomically adds committed traffic to the account's daily
// row. Callers pass only positive deltas; the zero case is intentionally a
// no-op so cancelled transfers that sent no bytes do not create rows.
func (d *DB) AddTelegramUsage(ctx context.Context, accountID, date string, upload, download int64) error {
	if upload < 0 || download < 0 {
		return errors.New("telegram usage deltas must not be negative")
	}
	if upload == 0 && download == 0 {
		return nil
	}
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO tg_account_usage (account_id, quota_date, upload_bytes, download_bytes)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (account_id, quota_date) DO UPDATE SET
		   upload_bytes = upload_bytes + excluded.upload_bytes,
		   download_bytes = download_bytes + excluded.download_bytes`,
		accountID, date, upload, download)
	return Translate(err)
}
