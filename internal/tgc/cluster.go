package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/indexer"
)

// Cluster holds every configured Telegram account.
//
// It is deliberately thin: it owns the set of Managers and the order they are
// offered in, and nothing else. Which account a given transfer runs on is
// decided in internal/drive, because that is where the task slots live and a
// scheduling decision without a slot to hand out is meaningless.
type Cluster struct {
	cfg *config.Config
	db  *database.DB
	log *zap.Logger

	mu       sync.RWMutex
	managers []*Manager

	// OnReady fires when any account becomes authorized, so work that was
	// waiting on Telegram resumes as soon as the first account can serve it.
	OnReady func()
}

var _ drive.Cluster = (*Cluster)(nil)

// ErrNoAccounts is returned when an operation needs a Telegram account and none
// has been configured yet.
var ErrNoAccounts = errors.New("tgc: no telegram account is configured")

// ErrLastAccount refuses to remove or disable the only account left, which
// would take the whole drive offline with no way back through the UI.
var ErrLastAccount = errors.New("tgc: the last enabled telegram account cannot be removed")

// NewCluster loads the configured accounts. It does not connect; call Start.
//
// A deployment upgrading from the single-account layout has its credentials in
// settings and its session in session.json; SeedPrimaryAccount adopts both as
// the primary account rather than asking the operator to sign in again.
func NewCluster(ctx context.Context, cfg *config.Config, db *database.DB, log *zap.Logger) (*Cluster, error) {
	settings := cfg.RuntimeSettings()
	if _, err := db.SeedPrimaryAccount(ctx, settings.AppID, settings.AppHash, database.LegacySessionFile); err != nil &&
		!errors.Is(err, database.ErrNotFound) {
		return nil, fmt.Errorf("adopt the existing telegram account: %w", err)
	}

	c := &Cluster{cfg: cfg, db: db, log: log}
	if err := c.reload(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// reload rebuilds the manager list from the database, keeping the managers for
// accounts that are still present so a change to one account never disturbs the
// connections of the others.
func (c *Cluster) reload(ctx context.Context) error {
	rows, err := c.db.ListAccounts(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	existing := make(map[string]*Manager, len(c.managers))
	for _, m := range c.managers {
		existing[m.ID()] = m
	}

	next := make([]*Manager, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if m, ok := existing[row.ID]; ok {
			delete(existing, row.ID)
			m.mu.Lock()
			m.acct = row
			m.mu.Unlock()
			next = append(next, m)
			continue
		}
		m := New(c.cfg, c.db, c.log, row)
		m.OnReady = c.readyCallback()
		next = append(next, m)
	}
	c.managers = next
	// Whatever is left in existing was disabled or deleted while running.
	stale := make([]*Manager, 0, len(existing))
	for _, m := range existing {
		stale = append(stale, m)
	}
	c.mu.Unlock()

	for _, m := range stale {
		m.Stop()
	}
	return nil
}

func (c *Cluster) readyCallback() func() {
	return func() {
		c.mu.RLock()
		fn := c.OnReady
		c.mu.RUnlock()
		if fn != nil {
			fn()
		}
	}
}

// Start connects every enabled account. One account failing to connect is not
// fatal for the others, and is not fatal at all: the WebUI has to come up so an
// administrator can fix it.
func (c *Cluster) Start(ctx context.Context) error {
	for _, m := range c.All() {
		if err := m.Start(ctx); err != nil {
			c.log.Error("could not connect a telegram account",
				zap.String("account", m.ID()), zap.Error(err))
		}
	}
	return nil
}

func (c *Cluster) Stop() {
	for _, m := range c.All() {
		m.Stop()
	}
}

// All returns every loaded account in scheduling order, connected or not.
func (c *Cluster) All() []*Manager {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]*Manager(nil), c.managers...)
}

// Primary is the account that runs the setup wizard, rebuilds the index and
// invites the others into the storage channel. It falls back to the first
// loaded account so a deployment whose primary row was removed still works.
func (c *Cluster) Primary() (*Manager, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.managers {
		if m.Account().IsPrimary {
			return m, nil
		}
	}
	if len(c.managers) > 0 {
		return c.managers[0], nil
	}
	return nil, ErrNoAccounts
}

// Manager returns one account by id.
func (c *Cluster) Manager(id string) (*Manager, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.managers {
		if m.ID() == id {
			return m, true
		}
	}
	return nil, false
}

// Ready reports whether at least one account can serve requests. A single
// throttled account still counts: the request will queue behind its
// FLOOD_WAIT rather than fail.
func (c *Cluster) Ready() bool {
	for _, m := range c.All() {
		if m.Ready() {
			return true
		}
	}
	return false
}

// Accounts implements drive.Cluster, returning the accounts that are signed in
// and not sitting out a FLOOD_WAIT.
//
// When every account is cooling down it returns the signed-in ones anyway:
// making the caller wait on a real account is better than telling it the drive
// is offline, and the waiter middleware absorbs the delay.
func (c *Cluster) Accounts() []drive.Account {
	managers := c.All()
	out := make([]drive.Account, 0, len(managers))
	for _, m := range managers {
		if m.Available() {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, m := range managers {
		if m.Ready() {
			out = append(out, m)
		}
	}
	return out
}

// Account implements drive.Cluster.
func (c *Cluster) Account(id string) (drive.Account, bool) {
	m, ok := c.Manager(id)
	if !ok {
		return nil, false
	}
	return m, true
}

// Add registers a new account and connects it. The account is not usable until
// it has signed in and been admitted to the storage channel; both are separate
// steps driven by the WebUI.
func (c *Cluster) Add(ctx context.Context, label string, appID int, appHash string) (*Manager, error) {
	if appID <= 0 || appHash == "" {
		return nil, errors.New("app id and app hash are both required")
	}

	existing, err := c.db.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	row := database.TGAccount{
		ID:      database.NewID(),
		Label:   label,
		AppID:   appID,
		AppHash: appHash,
		Enabled: true,
		// A dedicated session file per account. Reusing one would mean two
		// clients writing each other's auth key.
		Position: len(existing),
	}
	row.SessionFile = "session-" + row.ID + ".json"
	if len(existing) == 0 {
		// A drive with no accounts at all is being set up for the first time,
		// so this one takes the primary role and the historical session name.
		row.IsPrimary = true
		row.SessionFile = database.LegacySessionFile
	}
	if err := c.db.InsertAccount(ctx, row); err != nil {
		return nil, err
	}
	if err := c.reload(ctx); err != nil {
		return nil, err
	}

	m, ok := c.Manager(row.ID)
	if !ok {
		return nil, fmt.Errorf("account %s vanished immediately after being created", row.ID)
	}
	if err := m.Start(ctx); err != nil {
		// The row stays: the credentials may simply be wrong, and deleting it
		// here would lose the label and force the whole form to be retyped.
		c.log.Error("new telegram account could not connect", zap.Error(err))
	}
	return m, nil
}

// Remove disconnects an account, deletes its session file and forgets it.
//
// Segments it uploaded keep pointing at the gone account id. That is harmless:
// the reader treats an unknown owner as "no usable stored handle" and has the
// serving account re-resolve one, which is the same path a cross-account read
// already takes.
func (c *Cluster) Remove(ctx context.Context, id string) error {
	row, err := c.db.AccountByID(ctx, id)
	if err != nil {
		return err
	}
	enabled, err := c.db.CountEnabledAccounts(ctx)
	if err != nil {
		return err
	}
	if row.Enabled && enabled <= 1 {
		return ErrLastAccount
	}

	if m, ok := c.Manager(id); ok {
		// Log out rather than just disconnecting, so the authorization is
		// actually revoked on Telegram's side instead of lingering in the
		// account's device list.
		if err := m.Close(ctx); err != nil {
			c.log.Warn("could not log out a removed telegram account", zap.Error(err))
		}
	}
	if err := os.Remove(sessionPath(c.cfg, row)); err != nil && !os.IsNotExist(err) {
		c.log.Warn("could not remove a telegram session file", zap.Error(err))
	}
	if err := c.db.DeleteAccount(ctx, id); err != nil {
		return err
	}
	return c.reload(ctx)
}

// Update changes an account's label or enabled flag, reconnecting it when the
// change means it should now be running (or stopping it when it should not).
func (c *Cluster) Update(ctx context.Context, id, label string, enabled bool) error {
	row, err := c.db.AccountByID(ctx, id)
	if err != nil {
		return err
	}
	if row.Enabled && !enabled {
		count, err := c.db.CountEnabledAccounts(ctx)
		if err != nil {
			return err
		}
		if count <= 1 {
			return ErrLastAccount
		}
	}
	if err := c.db.UpdateAccountSettings(ctx, id, label, row.AppID, row.AppHash, enabled); err != nil {
		return err
	}
	if err := c.reload(ctx); err != nil {
		return err
	}
	if enabled && !row.Enabled {
		if m, ok := c.Manager(id); ok {
			return m.Start(ctx)
		}
	}
	return nil
}

// Reload re-reads the account list, used after a change made outside the
// cluster such as the settings page rewriting the primary's credentials.
func (c *Cluster) Reload(ctx context.Context) error { return c.reload(ctx) }

// PrimarySource adapts a cluster to the single-account interfaces that only
// ever want one connection: the index rebuild, which walks the channel with the
// access hash stored on the channel row, and the plugin host's status call.
//
// The lookup is deferred to each call rather than resolved once, so replacing
// the primary account does not leave a rebuild pointed at a dead client.
func PrimarySource(c *Cluster) *PrimaryRef { return &PrimaryRef{cluster: c} }

// PrimaryRef forwards to whichever account is currently primary.
type PrimaryRef struct{ cluster *Cluster }

var _ indexer.Source = (*PrimaryRef)(nil)

func (p *PrimaryRef) ID() string {
	m, err := p.cluster.Primary()
	if err != nil {
		return ""
	}
	return m.ID()
}

func (p *PrimaryRef) ScanHistory(
	ctx context.Context,
	ch drive.ChannelRef,
	visit func(indexer.Message) error,
) error {
	m, err := p.cluster.Primary()
	if err != nil {
		return err
	}
	return m.ScanHistory(ctx, ch, visit)
}

// Status reports the primary account's connection, which is what the setup
// wizard and the top-level status endpoint show.
func (p *PrimaryRef) Status() Status {
	m, err := p.cluster.Primary()
	if err != nil {
		return Status{State: StateUnconfigured}
	}
	return m.Status()
}
