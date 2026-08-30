package database

import (
	"context"
	"database/sql"
	"strings"
)

// Audit actions. They are constants rather than free text so that the log can
// be filtered reliably and so a typo cannot create a second spelling of an
// action that already exists.
const (
	AuditUserCreate      = "user.create"
	AuditUserDelete      = "user.delete"
	AuditUserUpdate      = "user.update"
	AuditUserRole        = "user.role"
	AuditUserPassword    = "user.password"
	AuditUserEnable      = "user.enable"
	AuditUserDisable     = "user.disable"
	AuditSessionRevoke   = "session.revoke"
	AuditSettingsUpdate  = "settings.update"
	AuditTelegramConfig  = "telegram.configure"
	AuditTelegramLogout  = "telegram.logout"
	AuditTelegramImport  = "telegram.import"
	AuditTelegramExport  = "telegram.export"
	AuditChannelSelect   = "telegram.channel"
	AuditIndexRebuild    = "index.rebuild"
	AuditShareCreate     = "share.create"
	AuditShareRevoke     = "share.revoke"
	AuditTransferDelete  = "transfer.delete"
	AuditDownloadStage   = "download.stage"
	AuditCachePurge      = "cache.purge"
	AuditFileDelete      = "file.delete"
	AuditFileBatchRename = "file.batchRename"
	AuditPluginInspect   = "plugin.inspect"
	AuditPluginInstall   = "plugin.install"
	AuditPluginEnable    = "plugin.enable"
	AuditPluginDisable   = "plugin.disable"
	AuditPluginUninstall = "plugin.uninstall"
)

// AppendAudit records one action. Callers treat a failure as non-fatal: losing
// an audit line is bad, but refusing the action the operator asked for because
// the log could not be written is worse.
func (d *DB) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	at := nowMS()
	if !e.At.IsZero() {
		at = e.At.UnixMilli()
	}
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO audit_log (id, at, actor_id, actor_name, action, target, detail, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, at, nullable(e.ActorID), e.ActorName, e.Action,
		truncate(e.Target, 512), truncate(e.Detail, 2048), e.IP)
	return Translate(err)
}

// AuditFilter narrows a query of the log.
type AuditFilter struct {
	ActorID string
	Action  string
	// From and To are Unix milliseconds; zero means unbounded.
	From, To int64
	Query    string
	Limit    int
}

// ListAudit returns matching entries, newest first.
func (d *DB) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	where := []string{"1 = 1"}
	args := []any{}

	if f.ActorID != "" {
		where = append(where, "actor_id = ?")
		args = append(args, f.ActorID)
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.From > 0 {
		where = append(where, "at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		where = append(where, "at <= ?")
		args = append(args, f.To)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(actor_name LIKE ? OR target LIKE ? OR detail LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args = append(args, limit)

	rows, err := d.read.QueryContext(ctx,
		`SELECT id, at, actor_id, actor_name, action, target, detail, ip FROM audit_log
		 WHERE `+strings.Join(where, " AND ")+` ORDER BY at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e       AuditEntry
			actor   sql.NullString
			atMilli int64
		)
		if err := rows.Scan(&e.ID, &atMilli, &actor, &e.ActorName, &e.Action, &e.Target, &e.Detail, &e.IP); err != nil {
			return nil, Translate(err)
		}
		e.At, e.ActorID = msToTime(atMilli), text(actor)
		out = append(out, e)
	}
	return out, Translate(rows.Err())
}

// PurgeAudit trims the log so it cannot grow without bound, like every other
// periodically-swept table here.
func (d *DB) PurgeAudit(ctx context.Context, olderThanMS int64) (int64, error) {
	res, err := d.write.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, olderThanMS)
	if err != nil {
		return 0, Translate(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
