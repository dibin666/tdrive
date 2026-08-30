package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"
)

// A share link is a bearer capability: whoever holds the token can read the
// file. That is the point — it has to work in aria2, in IDM, in a phone's
// browser, none of which will send an Authorization header — so the design
// leans on the same protections as the refresh token: 256 bits of entropy,
// only the hash is stored, and it can be revoked at any moment.

const shareCols = `id, user_id, file_id, kind, label, expires_at, revoked, hits, created_at, last_used_at`

func scanShare(row interface{ Scan(...any) error }) (ShareLink, error) {
	var (
		s                          ShareLink
		userID                     sql.NullString
		expires, created, lastUsed int64
		revoked                    int64
	)
	err := row.Scan(&s.ID, &userID, &s.FileID, &s.Kind, &s.Label,
		&expires, &revoked, &s.Hits, &created, &lastUsed)
	if err != nil {
		return ShareLink{}, Translate(err)
	}
	s.UserID = text(userID)
	s.Revoked = revoked != 0
	s.CreatedAt = msToTime(created)
	if expires > 0 {
		s.ExpiresAt = msToTime(expires)
	}
	if lastUsed > 0 {
		s.LastUsedAt = msToTime(lastUsed)
	}
	return s, nil
}

// NewShareToken mints the secret half of a link. It is returned once and never
// stored in plaintext.
func NewShareToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate share token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashShareToken is the stored form.
func HashShareToken(token string) []byte {
	sum := sha256.Sum256([]byte("tdrive-share\x00" + token))
	return sum[:]
}

// CreateShare records a link. expiresAt zero means the link never expires.
func (d *DB) CreateShare(ctx context.Context, s ShareLink, token string) (ShareLink, error) {
	if s.ID == "" {
		s.ID = NewID()
	}
	if s.Kind == "" {
		s.Kind = ShareFile
	}
	now := nowMS()
	s.CreatedAt = msToTime(now)

	var expires int64
	if !s.ExpiresAt.IsZero() {
		expires = s.ExpiresAt.UnixMilli()
	}

	_, err := d.write.ExecContext(ctx,
		`INSERT INTO share_links (id, user_id, file_id, token_hash, kind, label, expires_at, revoked, hits, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, 0)`,
		s.ID, nullable(s.UserID), s.FileID, HashShareToken(token), string(s.Kind), s.Label, expires, now)
	if err != nil {
		return ShareLink{}, fmt.Errorf("create share link: %w", Translate(err))
	}
	return s, nil
}

// ShareByToken resolves a token to its link, rejecting revoked and expired
// rows. It returns ErrNotFound for all three cases so a caller cannot use the
// error to distinguish "wrong token" from "revoked token".
func (d *DB) ShareByToken(ctx context.Context, token string) (ShareLink, error) {
	share, err := scanShare(d.read.QueryRowContext(ctx,
		`SELECT `+shareCols+` FROM share_links WHERE token_hash = ?`, HashShareToken(token)))
	if err != nil {
		return ShareLink{}, err
	}
	if share.Revoked {
		return ShareLink{}, ErrNotFound
	}
	if !share.ExpiresAt.IsZero() && time.Now().After(share.ExpiresAt) {
		return ShareLink{}, ErrNotFound
	}
	return share, nil
}

// TouchShare counts a use. It is fire-and-forget: a failed counter update must
// not fail the download it was counting.
func (d *DB) TouchShare(ctx context.Context, id string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE share_links SET hits = hits + 1, last_used_at = ? WHERE id = ?`, nowMS(), id)
	return Translate(err)
}

// ListShares returns a user's links, or every link when userID is empty, which
// is what an administrator sees.
func (d *DB) ListShares(ctx context.Context, userID string, includeRevoked bool) ([]ShareLink, error) {
	query := `SELECT ` + shareCols + ` FROM share_links WHERE 1 = 1`
	args := []any{}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if !includeRevoked {
		query += ` AND revoked = 0`
	}
	query += ` ORDER BY created_at DESC LIMIT 500`

	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []ShareLink
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, Translate(rows.Err())
}

func (d *DB) ShareByID(ctx context.Context, id string) (ShareLink, error) {
	return scanShare(d.read.QueryRowContext(ctx, `SELECT `+shareCols+` FROM share_links WHERE id = ?`, id))
}

// RevokeShare kills a link without deleting the record, so the audit trail
// still shows that it existed and how often it was used.
func (d *DB) RevokeShare(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `UPDATE share_links SET revoked = 1 WHERE id = ?`, id)
	return affectedOne(res, err, "revoke share link")
}

// PurgeShares removes links that expired or were revoked long enough ago that
// nobody is going to ask about them.
func (d *DB) PurgeShares(ctx context.Context, olderThanMS int64) (int64, error) {
	res, err := d.write.ExecContext(ctx,
		`DELETE FROM share_links
		 WHERE (expires_at > 0 AND expires_at < ?) OR (revoked = 1 AND created_at < ?)`,
		olderThanMS, olderThanMS)
	if err != nil {
		return 0, Translate(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
