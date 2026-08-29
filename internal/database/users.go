package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const userCols = `id, username, password_hash, role, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		u                User
		created, updated int64
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created, &updated)
	if err != nil {
		return User{}, Translate(err)
	}
	u.CreatedAt, u.UpdatedAt = msToTime(created), msToTime(updated)
	return u, nil
}

// CreateUser inserts an account. It returns ErrConflict when the username is
// taken, comparing case-insensitively so that "Admin" cannot shadow "admin".
func (d *DB) CreateUser(ctx context.Context, username, passwordHash string, role Role) (User, error) {
	now := nowMS()
	u := User{
		ID:           NewID(),
		Username:     username,
		PasswordHash: passwordHash,
		Role:         role,
		CreatedAt:    msToTime(now),
		UpdatedAt:    msToTime(now),
	}
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
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

// CountAdmins guards against deleting or demoting the last administrator,
// which would lock everyone out of the Telegram settings.
func (d *DB) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = ?`, string(RoleAdmin)).Scan(&n)
	return n, Translate(err)
}

// StoreRefreshToken records the hash of a refresh token. The plaintext is only
// ever in the client's cookie, so a database leak cannot mint sessions.
func (d *DB) StoreRefreshToken(ctx context.Context, userID string, hash []byte, expires time.Time) (string, error) {
	id := NewID()
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, revoked, created_at)
		 VALUES (?, ?, ?, ?, 0, ?)`,
		id, userID, hash, expires.UnixMilli(), nowMS())
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
