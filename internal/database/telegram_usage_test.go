package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTelegramAccountQuotasAndUsagePersist(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	account := TGAccount{
		ID:                 NewID(),
		Label:              "quota",
		AppID:              1234567,
		AppHash:            "hash",
		SessionFile:        "session-quota.json",
		Enabled:            true,
		UploadDailyQuota:   1000,
		DownloadDailyQuota: 2000,
	}
	if err := db.InsertAccount(ctx, account); err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}

	got, err := db.AccountByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if got.UploadDailyQuota != 1000 || got.DownloadDailyQuota != 2000 {
		t.Fatalf("quotas = upload %d/download %d, want 1000/2000",
			got.UploadDailyQuota, got.DownloadDailyQuota)
	}

	if err := db.AddTelegramUsage(ctx, account.ID, "2099-01-02", 100, 200); err != nil {
		t.Fatalf("first AddTelegramUsage: %v", err)
	}
	if err := db.AddTelegramUsage(ctx, account.ID, "2099-01-02", 50, 75); err != nil {
		t.Fatalf("second AddTelegramUsage: %v", err)
	}
	usage, err := db.TelegramUsageFor(ctx, account.ID, "2099-01-02")
	if err != nil {
		t.Fatalf("TelegramUsageFor: %v", err)
	}
	if usage.UploadBytes != 150 || usage.DownloadBytes != 275 {
		t.Fatalf("usage = upload %d/download %d, want 150/275",
			usage.UploadBytes, usage.DownloadBytes)
	}
}

func TestTelegramQuotaSchemaMigratesFromPreviousVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quota-migration.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open initial database: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE tg_account_usage`,
		`ALTER TABLE tg_accounts DROP COLUMN upload_daily_quota`,
		`ALTER TABLE tg_accounts DROP COLUMN download_daily_quota`,
		`PRAGMA user_version = 8`,
	} {
		if _, err := db.Write().ExecContext(ctx, stmt); err != nil {
			db.Close()
			t.Fatalf("prepare previous schema with %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close previous schema: %v", err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open migrated database: %v", err)
	}
	t.Cleanup(func() { migrated.Close() })
	account := TGAccount{
		ID:                 NewID(),
		AppID:              1234567,
		AppHash:            "hash",
		SessionFile:        "session-migrated-quota.json",
		Enabled:            true,
		UploadDailyQuota:   300,
		DownloadDailyQuota: 400,
	}
	if err := migrated.InsertAccount(ctx, account); err != nil {
		t.Fatalf("InsertAccount after migration: %v", err)
	}
	got, err := migrated.AccountByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("AccountByID after migration: %v", err)
	}
	if got.UploadDailyQuota != 300 || got.DownloadDailyQuota != 400 {
		t.Fatalf("migrated quotas = %d/%d, want 300/400",
			got.UploadDailyQuota, got.DownloadDailyQuota)
	}
}
