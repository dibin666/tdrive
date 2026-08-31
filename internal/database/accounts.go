package database

import (
	"context"
	"errors"
	"fmt"
)

// Telegram accounts and their per-account view of a storage channel.
//
// Telegram meters FLOOD_WAIT and transfer quota per account, so the only way
// to get a second budget is a second login — several api_id values on one
// phone number share the same budget and buy nothing. Everything here exists
// to keep those logins genuinely separate: separate credentials, separate
// session files, and separate access hashes, because Telegram mints an access
// hash for the requesting account and the value one account holds is
// meaningless to another.

const accountCols = `id, label, app_id, app_hash, proxy_url, session_file, enabled, is_primary,
	tg_user_id, username, phone, position, created_at`

func scanAccount(row interface{ Scan(...any) error }) (TGAccount, error) {
	var (
		a         TGAccount
		enabled   int64
		isPrimary int64
		created   int64
	)
	err := row.Scan(&a.ID, &a.Label, &a.AppID, &a.AppHash, &a.ProxyURL, &a.SessionFile,
		&enabled, &isPrimary, &a.TGUserID, &a.Username, &a.Phone, &a.Position, &created)
	if err != nil {
		return TGAccount{}, Translate(err)
	}
	a.Enabled = enabled != 0
	a.IsPrimary = isPrimary != 0
	a.CreatedAt = msToTime(created)
	return a, nil
}

// ListAccounts returns every account in scheduling order. The order is stable
// so that round-robin over it is predictable across restarts.
func (d *DB) ListAccounts(ctx context.Context) ([]TGAccount, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+accountCols+` FROM tg_accounts ORDER BY is_primary DESC, position, created_at`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []TGAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, Translate(rows.Err())
}

func (d *DB) AccountByID(ctx context.Context, id string) (TGAccount, error) {
	return scanAccount(d.read.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM tg_accounts WHERE id = ?`, id))
}

// PrimaryAccount is the one that runs the setup wizard, rebuilds the index and
// invites the others into the storage channel.
func (d *DB) PrimaryAccount(ctx context.Context) (TGAccount, error) {
	return scanAccount(d.read.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM tg_accounts WHERE is_primary = 1 LIMIT 1`))
}

func (d *DB) InsertAccount(ctx context.Context, a TGAccount) error {
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO tg_accounts (`+accountCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Label, a.AppID, a.AppHash, a.ProxyURL, a.SessionFile, boolInt(a.Enabled),
		boolInt(a.IsPrimary), a.TGUserID, a.Username, a.Phone, a.Position, nowMS())
	if err != nil {
		return fmt.Errorf("insert telegram account: %w", Translate(err))
	}
	return nil
}

// UpdateAccountSettings changes the parts an administrator edits. The session
// file and the primary flag are deliberately not writable here: moving either
// one means re-authenticating, which is a different operation.
func (d *DB) UpdateAccountSettings(ctx context.Context, id, label string, appID int, appHash string, enabled bool) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE tg_accounts SET label = ?, app_id = ?, app_hash = ?, enabled = ? WHERE id = ?`,
		label, appID, appHash, boolInt(enabled), id)
	return affectedOne(res, err, "update telegram account")
}

// SetAccountProxy points one account at an outbound proxy, or at nothing when
// proxyURL is empty. It is separate from UpdateAccountSettings because changing
// it means tearing down and redialling the Telegram connection, which the
// caller has to do afterwards.
func (d *DB) SetAccountProxy(ctx context.Context, id, proxyURL string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE tg_accounts SET proxy_url = ? WHERE id = ?`, proxyURL, id)
	return affectedOne(res, err, "set the proxy of a telegram account")
}

// SetAccountIdentity caches who an account signed in as, so the accounts list
// can name it without a live connection.
func (d *DB) SetAccountIdentity(ctx context.Context, id string, tgUserID int64, username, phone string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE tg_accounts SET tg_user_id = ?, username = ?, phone = ? WHERE id = ?`,
		tgUserID, username, phone, id)
	return Translate(err)
}

func (d *DB) DeleteAccount(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `DELETE FROM tg_accounts WHERE id = ?`, id)
	return affectedOne(res, err, "delete telegram account")
}

// CountEnabledAccounts backs the rule that the last usable account cannot be
// removed or disabled.
func (d *DB) CountEnabledAccounts(ctx context.Context) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx,
		`SELECT count(*) FROM tg_accounts WHERE enabled = 1`).Scan(&n)
	return n, Translate(err)
}

// SeedPrimaryAccount turns a single-account deployment into the first row of a
// multi-account one, and is a no-op once any account exists.
//
// It also carries the two things that used to be implicitly the only account's:
// the channel access hashes stored on channels, and the ownership of every
// segment already uploaded. Without that backfill, a reader would treat every
// existing segment as belonging to nobody and re-resolve handles it already
// has.
func (d *DB) SeedPrimaryAccount(ctx context.Context, appID int, appHash, sessionFile string) (TGAccount, error) {
	if existing, err := d.PrimaryAccount(ctx); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return TGAccount{}, err
	}
	// An account row with no credentials would only fail to connect. A fresh
	// install has none yet; the setup wizard creates the primary later.
	if appID == 0 || appHash == "" {
		return TGAccount{}, ErrNotFound
	}
	if sessionFile == "" {
		sessionFile = LegacySessionFile
	}

	account := TGAccount{
		ID:          NewID(),
		Label:       "主账号",
		AppID:       appID,
		AppHash:     appHash,
		SessionFile: sessionFile,
		Enabled:     true,
		IsPrimary:   true,
	}
	err := d.Tx(ctx, func(tx txExec) error {
		// Re-check inside the transaction: two processes opening the same data
		// directory would otherwise both conclude they are the first.
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tg_accounts`).Scan(&n); err != nil {
			return Translate(err)
		}
		if n > 0 {
			return errAlreadySeeded
		}
		// The literals are proxy_url, enabled, is_primary, tg_user_id,
		// username, phone and position: an adopted account connects directly
		// and has no cached identity until it next signs in.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tg_accounts (`+accountCols+`) VALUES (?, ?, ?, ?, '', ?, 1, 1, 0, '', '', 0, ?)`,
			account.ID, account.Label, account.AppID, account.AppHash,
			account.SessionFile, nowMS()); err != nil {
			return Translate(err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_accounts (channel_id, account_id, access_hash, can_post, checked_at)
			 SELECT id, ?, access_hash, 1, 0 FROM channels`, account.ID); err != nil {
			return Translate(err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE segments SET account_id = ? WHERE account_id = ''`, account.ID); err != nil {
			return Translate(err)
		}
		return nil
	})
	if errors.Is(err, errAlreadySeeded) {
		return d.PrimaryAccount(ctx)
	}
	if err != nil {
		return TGAccount{}, err
	}
	account.CreatedAt = msToTime(nowMS())
	return account, nil
}

// LegacySessionFile is the name a single-account deployment used, kept so that
// an upgrade does not have to sign in again.
const LegacySessionFile = "session.json"

var errAlreadySeeded = errors.New("database: telegram accounts already seeded")

const channelAccessCols = `channel_id, account_id, access_hash, can_post, checked_at`

func scanChannelAccess(row interface{ Scan(...any) error }) (ChannelAccess, error) {
	var (
		a       ChannelAccess
		canPost int64
		checked int64
	)
	if err := row.Scan(&a.ChannelID, &a.AccountID, &a.AccessHash, &canPost, &checked); err != nil {
		return ChannelAccess{}, Translate(err)
	}
	a.CanPost = canPost != 0
	a.CheckedAt = msToTime(checked)
	return a, nil
}

// UpsertChannelAccess records what one account resolved for one channel. It is
// written every time an account joins a channel or re-resolves it, because an
// access hash can rotate and a stale one fails every later request.
func (d *DB) UpsertChannelAccess(ctx context.Context, channelID, accountID string, accessHash int64, canPost bool) error {
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO channel_accounts (`+channelAccessCols+`) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (channel_id, account_id) DO UPDATE SET
		   access_hash = excluded.access_hash, can_post = excluded.can_post,
		   checked_at = excluded.checked_at`,
		channelID, accountID, accessHash, boolInt(canPost), nowMS())
	if err != nil {
		return fmt.Errorf("record channel access for account %s: %w", accountID, Translate(err))
	}
	return nil
}

func (d *DB) ChannelAccessFor(ctx context.Context, channelID, accountID string) (ChannelAccess, error) {
	return scanChannelAccess(d.read.QueryRowContext(ctx,
		`SELECT `+channelAccessCols+` FROM channel_accounts WHERE channel_id = ? AND account_id = ?`,
		channelID, accountID))
}

// DeleteChannelAccess forgets a membership that Telegram no longer reports.
// Keeping the row would make the settings page say "in channel" forever and
// would let a later scheduler pass use an access hash for a removed account.
func (d *DB) DeleteChannelAccess(ctx context.Context, channelID, accountID string) error {
	_, err := d.write.ExecContext(ctx,
		`DELETE FROM channel_accounts WHERE channel_id = ? AND account_id = ?`,
		channelID, accountID)
	return Translate(err)
}

// ChannelAccesses lists every account's view of one channel, used by the
// settings page to show which accounts are actually able to store into it.
func (d *DB) ChannelAccesses(ctx context.Context, channelID string) ([]ChannelAccess, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+channelAccessCols+` FROM channel_accounts WHERE channel_id = ?`, channelID)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []ChannelAccess
	for rows.Next() {
		a, err := scanChannelAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, Translate(rows.Err())
}
