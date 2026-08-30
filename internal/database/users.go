package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const userCols = `id, username, password_hash, role, created_at, updated_at,
	enabled, perms, scope_path, quota_bytes, note, last_login_at, last_login_ip`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		u                  User
		created, updated   int64
		enabled, lastLogin int64
		perms              int64
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &updated,
		&enabled, &perms, &u.ScopePath, &u.QuotaBytes, &u.Note, &lastLogin, &u.LastLoginIP)
	if err != nil {
		return User{}, Translate(err)
	}
	u.CreatedAt, u.UpdatedAt = msToTime(created), msToTime(updated)
	u.Enabled = enabled != 0
	u.Perms = Perm(perms)
	if lastLogin > 0 {
		u.LastLoginAt = msToTime(lastLogin)
	}
	return u, nil
}

// CreateUser inserts an account. It returns ErrConflict when the username is
// taken, comparing case-insensitively so that "Admin" cannot shadow "admin".
//
// Perms is left at zero, which means "follow the role". An account only gets a
// stored mask once someone customises it, so the common case cannot drift out
// of sync with what the role is supposed to mean.
func (d *DB) CreateUser(ctx context.Context, username, passwordHash string, role Role) (User, error) {
	now := nowMS()
	u := User{
		ID:           NewID(),
		Username:     username,
		PasswordHash: passwordHash,
		Role:         role,
		CreatedAt:    msToTime(now),
		UpdatedAt:    msToTime(now),
		Enabled:      true,
	}
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, created_at, updated_at, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, 1)`,
		u.ID, u.Username, u.PasswordHash, string(u.Role), now, now)
	if err != nil {
		return User{}, fmt.Errorf("create user %q: %w", username, Translate(err))
	}
	return u, nil
}

func (d *DB) UserByName(ctx context.Context, username string) (User, error) {
	row := d.read.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE username = ? COLLATE NOCASE`, username)
	return scanUser(row)
}

func (d *DB) UserByID(ctx context.Context, id string) (User, error) {
	row := d.read.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, Translate(rows.Err())
}

// CountUsers drives the first-run setup wizard: zero users means the WebUI
// should ask for an admin account instead of a login.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, Translate(err)
}

// UserProfile is the set of administrative fields that can be edited together.
// Every field is a pointer so that a PATCH-shaped request can leave the rest
// alone: sending only "disable this account" must not silently reset a quota
// somebody configured last month.
type UserProfile struct {
	Enabled    *bool
	Perms      *Perm
	ScopePath  *string
	QuotaBytes *int64
	Note       *string
}

// UpdateUserProfile applies whichever fields were supplied.
func (d *DB) UpdateUserProfile(ctx context.Context, id string, p UserProfile) error {
	sets := []string{"updated_at = ?"}
	args := []any{nowMS()}

	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolInt(*p.Enabled))
	}
	if p.Perms != nil {
		sets = append(sets, "perms = ?")
		args = append(args, int64(*p.Perms))
	}
	if p.ScopePath != nil {
		sets = append(sets, "scope_path = ?")
		args = append(args, *p.ScopePath)
	}
	if p.QuotaBytes != nil {
		sets = append(sets, "quota_bytes = ?")
		args = append(args, *p.QuotaBytes)
	}
	if p.Note != nil {
		sets = append(sets, "note = ?")
		args = append(args, *p.Note)
	}
	if len(sets) == 1 {
		// Nothing but the timestamp would change; treat it as a no-op rather
		// than bumping updated_at for an empty request.
		return nil
	}

	args = append(args, id)
	res, err := d.write.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return affectedOne(res, err, "update user")
}

// TouchLogin records a successful sign-in. It is best-effort by design: a
// failure here must never turn a valid login into a rejected one, so callers
// log rather than propagate.
func (d *DB) TouchLogin(ctx context.Context, id, ip string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE users SET last_login_at = ?, last_login_ip = ? WHERE id = ?`, nowMS(), ip, id)
	return Translate(err)
}

// UsedBytesByOwner totals the storage an account is responsible for. Pending
// uploads are included so that two large uploads started at once cannot both
// pass a quota check that only one of them fits under.
func (d *DB) UsedBytesByOwner(ctx context.Context, ownerID string) (int64, error) {
	if ownerID == "" {
		return 0, nil
	}
	var total sql.NullInt64
	err := d.read.QueryRowContext(ctx,
		`SELECT sum(size) FROM files WHERE owner_id = ? AND status <> 'broken'`, ownerID).Scan(&total)
	if err != nil {
		return 0, Translate(err)
	}
	return total.Int64, nil
}

// UsageByOwner returns bytes and file counts for every account in one pass, so
// the admin user list does not issue a query per row.
type Usage struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
}

func (d *DB) UsageByOwner(ctx context.Context) (map[string]Usage, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT owner_id, sum(size), count(*) FROM files
		 WHERE owner_id IS NOT NULL AND status <> 'broken' GROUP BY owner_id`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	out := make(map[string]Usage)
	for rows.Next() {
		var (
			owner sql.NullString
			u     Usage
		)
		if err := rows.Scan(&owner, &u.Bytes, &u.Files); err != nil {
			return nil, Translate(err)
		}
		out[text(owner)] = u
	}
	return out, Translate(rows.Err())
}

func (d *DB) SetUserPassword(ctx context.Context, id, passwordHash string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, nowMS(), id)
	return affectedOne(res, err, "update password")
}

func (d *DB) SetUserRole(ctx context.Context, id string, role Role) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`, string(role), nowMS(), id)
	return affectedOne(res, err, "update role")
}

// DeleteUser removes an account and, through ON DELETE CASCADE, its refresh
// tokens. Callers must ensure at least one admin remains.
func (d *DB) DeleteUser(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return affectedOne(res, err, "delete user")
}

// CountAdmins guards against deleting, demoting or disabling the last
// administrator, which would lock everyone out of the Telegram settings. Only
// enabled administrators count: a disabled one cannot log in to fix anything,
// so it is not a way back in.
func (d *DB) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = ? AND enabled = 1`, string(RoleAdmin)).Scan(&n)
	return n, Translate(err)
}

// StoreRefreshToken records the hash of a refresh token. The plaintext is only
// ever in the client's cookie, so a database leak cannot mint sessions.
//
// The user agent and IP are stored so that the session list can describe a
// device instead of showing a row of opaque ids. They are display-only and are
// never part of an authentication decision.
func (d *DB) StoreRefreshToken(ctx context.Context, userID string, hash []byte, expires time.Time, userAgent, ip string) (string, error) {
	id := NewID()
	now := nowMS()
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO refresh_tokens
		 (id, user_id, token_hash, expires_at, revoked, created_at, user_agent, ip, last_used_at)
		 VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		id, userID, hash, expires.UnixMilli(), now, truncate(userAgent, 255), ip, now)
	if err != nil {
		return "", fmt.Errorf("store refresh token: %w", Translate(err))
	}
	return id, nil
}

// LookupRefreshToken resolves a token hash to its user, rejecting revoked and
// expired rows.
func (d *DB) LookupRefreshToken(ctx context.Context, hash []byte) (userID, tokenID string, err error) {
	var expires int64
	var revoked int
	row := d.read.QueryRowContext(ctx,
		`SELECT id, user_id, expires_at, revoked FROM refresh_tokens WHERE token_hash = ?`, hash)
	if err := row.Scan(&tokenID, &userID, &expires, &revoked); err != nil {
		return "", "", Translate(err)
	}
	if revoked != 0 || time.Now().UnixMilli() > expires {
		return "", "", ErrNotFound
	}
	return userID, tokenID, nil
}

// ListSessions returns an account's live sessions, most recently used first.
func (d *DB) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id, user_id, user_agent, ip, created_at, last_used_at, expires_at
		 FROM refresh_tokens
		 WHERE user_id = ? AND revoked = 0 AND expires_at > ?
		 ORDER BY last_used_at DESC`, userID, time.Now().UnixMilli())
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			s                         Session
			created, lastUsed, expiry int64
		)
		if err := rows.Scan(&s.ID, &s.UserID, &s.UserAgent, &s.IP, &created, &lastUsed, &expiry); err != nil {
			return nil, Translate(err)
		}
		s.CreatedAt, s.ExpiresAt = msToTime(created), msToTime(expiry)
		if lastUsed > 0 {
			s.LastUsedAt = msToTime(lastUsed)
		}
		out = append(out, s)
	}
	return out, Translate(rows.Err())
}

// RevokeSessionOf revokes one session, refusing to touch a session that does
// not belong to the named account. Scoping the delete in SQL rather than
// checking first removes the window between the check and the write.
func (d *DB) RevokeSessionOf(ctx context.Context, userID, sessionID string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = 1 WHERE id = ? AND user_id = ?`, sessionID, userID)
	return affectedOne(res, err, "revoke session")
}

// TouchSession records that a refresh token was just used, so the session list
// can show real activity rather than only when the session began.
func (d *DB) TouchSession(ctx context.Context, tokenID string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE refresh_tokens SET last_used_at = ? WHERE id = ?`, nowMS(), tokenID)
	return Translate(err)
}

func (d *DB) RevokeRefreshToken(ctx context.Context, tokenID string) error {
	_, err := d.write.ExecContext(ctx, `UPDATE refresh_tokens SET revoked = 1 WHERE id = ?`, tokenID)
	return Translate(err)
}

// RevokeUserTokens logs an account out everywhere, used when its password or
// role changes.
func (d *DB) RevokeUserTokens(ctx context.Context, userID string) error {
	_, err := d.write.ExecContext(ctx, `UPDATE refresh_tokens SET revoked = 1 WHERE user_id = ?`, userID)
	return Translate(err)
}

// PurgeExpiredTokens is called periodically so the table does not grow without
// bound.
func (d *DB) PurgeExpiredTokens(ctx context.Context) (int64, error) {
	res, err := d.write.ExecContext(ctx,
		`DELETE FROM refresh_tokens WHERE expires_at < ? OR revoked = 1`, time.Now().UnixMilli())
	if err != nil {
		return 0, Translate(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func affectedOne(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, Translate(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
