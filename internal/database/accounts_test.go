package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Adopting a single-account deployment is the migration that has to be right:
// getting it wrong means an upgrade that asks the operator to sign in again,
// loses every channel access hash, and makes every existing segment look like
// it belongs to nobody.
func TestSeedPrimaryAccountAdoptsAnExistingDeployment(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	// The shape a pre-accounts install is in: credentials in settings, a
	// channel with the account's access hash, segments with no owner.
	if err := db.SetSettings(ctx, map[string]string{
		SettingTGAppID:   "1234567",
		SettingTGAppHash: "0123456789abcdef0123456789abcdef",
	}); err != nil {
		t.Fatalf("seed legacy settings: %v", err)
	}
	channel, err := db.UpsertChannel(ctx, -1001234567890, 999, "drive")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	file := seedFileWithSegment(t, db, channel)

	account, err := db.SeedPrimaryAccount(ctx, 1234567, "0123456789abcdef0123456789abcdef", LegacySessionFile)
	if err != nil {
		t.Fatalf("SeedPrimaryAccount: %v", err)
	}
	if !account.IsPrimary || !account.Enabled {
		t.Fatalf("the adopted account is not an enabled primary: %+v", account)
	}
	if account.SessionFile != LegacySessionFile {
		t.Fatalf("session file = %q, want %q — an upgrade must not have to sign in again",
			account.SessionFile, LegacySessionFile)
	}

	// The channel access hash the single account was using has to become that
	// account's per-account hash, or its next request fails.
	access, err := db.ChannelAccessFor(ctx, channel.ID, account.ID)
	if err != nil {
		t.Fatalf("the adopted account has no access to the existing channel: %v", err)
	}
	if access.AccessHash != 999 || !access.CanPost {
		t.Fatalf("channel access = %+v, want the stored hash 999 with posting rights", access)
	}

	// Existing segments must be attributed to it, otherwise every read
	// re-resolves a handle the database already holds.
	segs, err := db.Segments(ctx, file)
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if len(segs) != 1 || segs[0].AccountID != account.ID {
		t.Fatalf("segment ownership = %q, want the adopted account %q",
			segs[0].AccountID, account.ID)
	}
}

// Running twice must not produce a second primary; startup calls it on every
// boot.
func TestSeedPrimaryAccountIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	first, err := db.SeedPrimaryAccount(ctx, 1234567, "hash", LegacySessionFile)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	second, err := db.SeedPrimaryAccount(ctx, 7654321, "other", "session-other.json")
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("a second seed created account %s alongside %s", second.ID, first.ID)
	}
	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts after seeding twice, want 1", len(accounts))
	}
}

// A fresh install has no credentials anywhere. Inventing an account row for it
// would only produce one that can never connect.
func TestSeedPrimaryAccountSkipsAFreshInstall(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, err := db.SeedPrimaryAccount(ctx, 0, "", LegacySessionFile); !errors.Is(err, ErrNotFound) {
		t.Fatalf("seeding without credentials returned %v, want ErrNotFound", err)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedFileWithSegment writes one file with one unowned segment and returns its
// id, standing in for data uploaded before accounts existed.
func seedFileWithSegment(t *testing.T, db *DB, channel Channel) string {
	t.Helper()
	ctx := context.Background()

	file := File{
		ID: NewID(), Name: "old.bin", Size: 10, SegmentSize: 10, SegmentCount: 1,
		Status: StatusComplete, ChannelID: channel.ID,
	}
	if err := db.InsertFile(ctx, file); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	if err := db.UpsertSegment(ctx, Segment{
		FileID: file.ID, Index: 1, Size: 10, TGMsgID: 7, TGDocID: 8,
		AccessHash: 9, DCID: 2, FileReference: []byte("ref"),
	}); err != nil {
		t.Fatalf("UpsertSegment: %v", err)
	}
	return file.ID
}
