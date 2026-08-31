// Package tgc owns the Telegram connections: their lifecycle, their connection
// pools and the login flow that the WebUI drives.
//
// gotd's Client.Run blocks for as long as the connection should live, which
// suits a CLI but not a server that has to answer HTTP requests the whole time.
// Manager therefore runs it on its own goroutine and publishes a snapshot of
// the connection state that handlers can read without blocking.
//
// One Manager is one Telegram account. A deployment may hold several, held
// together by Cluster: Telegram meters FLOOD_WAIT and transfer quota per
// account, so a second login is the only way to get a second budget. Nothing
// is shared between them — separate credentials, separate session files,
// separate pools, separate rate limiters — which is what lets one sit out a
// FLOOD_WAIT while the others keep working.
package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	"github.com/gotd/log/logzap"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
)

// State describes how far the Telegram connection has got. The WebUI turns
// this directly into which step of the setup wizard to show.
type State string

const (
	// StateUnconfigured means no api_id/api_hash is known yet.
	StateUnconfigured State = "unconfigured"
	// StateConnecting means the client is dialling.
	StateConnecting State = "connecting"
	// StateUnauthorized means connected but not signed in: the phone/code
	// flow can run.
	StateUnauthorized State = "unauthorized"
	// StateReady means signed in and usable.
	StateReady State = "ready"
	// StateError means the run loop stopped with an error.
	StateError State = "error"
)

var (
	// ErrNotConfigured is returned before app credentials are known.
	ErrNotConfigured = errors.New("tgc: telegram app credentials are not configured")
	// ErrNotReady is returned when a call needs an authorized connection.
	ErrNotReady = errors.New("tgc: telegram client is not signed in")
)

// Status is the snapshot handed to the API layer.
type Status struct {
	State        State  `json:"state"`
	Error        string `json:"error,omitempty"`
	UserID       int64  `json:"userId,omitempty"`
	Username     string `json:"username,omitempty"`
	FirstName    string `json:"firstName,omitempty"`
	Phone        string `json:"phone,omitempty"`
	Premium      bool   `json:"premium"`
	DC           int    `json:"dc,omitempty"`
	AwaitingCode bool   `json:"awaitingCode"`
	// AwaitingPassword means the account has 2FA and the code was accepted.
	AwaitingPassword bool `json:"awaitingPassword"`
	// CooldownMs is how long Telegram has told this account to wait before
	// sending more requests. Non-zero means the scheduler is routing new work
	// to the other accounts.
	CooldownMs int64 `json:"cooldownMs,omitempty"`
}

// Manager owns one Telegram account's connection.
type Manager struct {
	cfg *config.Config
	db  *database.DB
	log *zap.Logger

	mu     sync.RWMutex
	acct   database.TGAccount
	state  State
	runErr error
	client *telegram.Client
	pool   Pool
	self   *tg.User

	// coolUntil is a Unix-milli deadline set by the health middleware when
	// Telegram answers with FLOOD_WAIT. It is read without the mutex on every
	// scheduling decision, which is why it is atomic rather than guarded.
	coolUntil atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}

	// OnReady is invoked once when a connection becomes authorized. It lets
	// server-side work that was waiting for Telegram (such as staged
	// downloads) resume after a late setup or reconnect.
	OnReady func()

	// Login flow state. Telegram hands back a phone_code_hash with the sent
	// code and expects it echoed on sign-in, so it has to survive between two
	// unrelated HTTP requests.
	loginPhone    string
	loginCodeHash string
	awaitingPass  bool
}

// New builds a manager for one account. It does not connect; call Start.
func New(cfg *config.Config, db *database.DB, log *zap.Logger, acct database.TGAccount) *Manager {
	return &Manager{
		cfg:   cfg,
		db:    db,
		log:   log.With(zap.String("account", acct.ID)),
		acct:  acct,
		state: StateUnconfigured,
	}
}

// ID identifies this account across the drive, the scheduler and the segments
// table.
func (m *Manager) ID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.acct.ID
}

// Account returns the stored row backing this connection.
func (m *Manager) Account() database.TGAccount {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.acct
}

// SessionPath is where this account's gotd session lives. Every account gets
// its own file: sharing one would mean two clients overwriting each other's
// auth key.
func (m *Manager) SessionPath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sessionPath(m.cfg, m.acct)
}

func sessionPath(cfg *config.Config, acct database.TGAccount) string {
	name := acct.SessionFile
	if name == "" {
		name = database.LegacySessionFile
	}
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(cfg.Server.DataDir, name)
}

// Credentials returns this account's api_id/api_hash pair.
func (m *Manager) Credentials(context.Context) (int, string, error) {
	m.mu.RLock()
	appID, appHash := m.acct.AppID, m.acct.AppHash
	m.mu.RUnlock()
	if appID == 0 || appHash == "" {
		return 0, "", ErrNotConfigured
	}
	return appID, appHash, nil
}

// Cooldown is how much of a FLOOD_WAIT this account still has to sit out. Zero
// means it is free to take work.
func (m *Manager) Cooldown() time.Duration {
	remaining := time.UnixMilli(m.coolUntil.Load()).Sub(time.Now())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// Available reports whether the scheduler may hand this account new work: it
// is signed in and not sitting out a FLOOD_WAIT.
func (m *Manager) Available() bool {
	return m.Ready() && m.Cooldown() == 0
}

// markFlood records a FLOOD_WAIT so the scheduler stops choosing this account
// until it expires. The waiter middleware still absorbs the wait for requests
// already in flight; this only steers new ones elsewhere.
func (m *Manager) markFlood(wait time.Duration) {
	if wait <= 0 {
		return
	}
	until := time.Now().Add(wait).UnixMilli()
	for {
		current := m.coolUntil.Load()
		if current >= until || m.coolUntil.CompareAndSwap(current, until) {
			return
		}
	}
}

// Start connects and, if a stored session is still valid, comes up signed in.
// Missing credentials are not an error: the WebUI shows the setup wizard and
// calls Configure later.
func (m *Manager) Start(ctx context.Context) error {
	appID, appHash, err := m.Credentials(ctx)
	if errors.Is(err, ErrNotConfigured) {
		m.setState(StateUnconfigured, nil)
		m.log.Info("telegram credentials not configured yet; waiting for setup")
		return nil
	}
	if err != nil {
		return err
	}
	return m.start(ctx, appID, appHash)
}

func (m *Manager) start(ctx context.Context, appID int, appHash string) error {
	m.Stop()
	settings := m.cfg.RuntimeSettings()
	sessionFile := m.SessionPath()

	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}

	// Rate limiting smooths bursts so Telegram is less likely to answer with
	// FLOOD_WAIT at all; the waiter then absorbs the ones that still happen,
	// which is what the reference implementation does by hand around every
	// send_message call.
	//
	// The health observer sits inside the waiter so that it sees the raw
	// FLOOD_WAIT before the waiter retries it away. That is the signal the
	// scheduler needs to start sending new transfers to another account.
	//
	// Every one of these is per-account: the rate limiter, the retry budget and
	// the connection pool below all belong to this login alone, which is the
	// whole point of running more than one.
	middlewares := []telegram.Middleware{
		floodwait.NewSimpleWaiter().WithMaxRetries(5).WithMaxWait(5 * time.Minute),
		m.healthMiddleware(),
		ratelimit.New(rate.Every(settings.RateLimit), m.cfg.Telegram.RateBurst),
	}

	client := telegram.NewClient(appID, appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionFile},
		Middlewares:    middlewares,
		Device: telegram.DeviceConfig{
			DeviceModel:   "tdrive",
			SystemVersion: "linux",
			AppVersion:    Version,
		},
		RetryInterval: 2 * time.Second,
		MaxRetries:    -1, // reconnect forever; a server should outlive a blip
		DialTimeout:   15 * time.Second,
		Logger:        logzap.New(m.log.Named("mtproto")),
	})

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	connected := make(chan error, 1)
	done := make(chan struct{})

	m.mu.Lock()
	m.client = client
	m.cancel = cancel
	m.done = done
	m.state = StateConnecting
	m.runErr = nil
	m.pool = NewPool(client, settings.PoolSize, middlewares...)
	m.mu.Unlock()

	go func() {
		defer close(done)
		err := client.Run(runCtx, func(ctx context.Context) error {
			connected <- nil
			// Hold the connection open until Stop cancels runCtx.
			<-ctx.Done()
			return ctx.Err()
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			m.log.Error("telegram run loop stopped", zap.Error(err))
			m.setState(StateError, err)
			select {
			case connected <- err:
			default:
			}
		}
	}()

	select {
	case err := <-connected:
		if err != nil {
			return fmt.Errorf("connect to telegram: %w", err)
		}
	case <-time.After(45 * time.Second):
		m.Stop()
		return errors.New("timed out connecting to telegram")
	case <-ctx.Done():
		m.Stop()
		return ctx.Err()
	}

	m.refreshAuth(runCtx)
	return nil
}

// refreshAuth asks Telegram whether the stored session is still signed in and
// caches the account for the status endpoint.
func (m *Manager) refreshAuth(ctx context.Context) {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()
	if client == nil {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	status, err := client.Auth().Status(callCtx)
	if err != nil {
		m.log.Warn("could not read telegram auth status", zap.Error(err))
		m.setState(StateUnauthorized, nil)
		return
	}

	m.mu.Lock()
	becameReady := false
	var onReady func()
	accountID := m.acct.ID
	if status.Authorized {
		becameReady = m.state != StateReady
		m.self = status.User
		m.state = StateReady
		m.loginPhone, m.loginCodeHash, m.awaitingPass = "", "", false
		m.acct.TGUserID = status.User.GetID()
		m.acct.Username = status.User.Username
		if status.User.Phone != "" {
			m.acct.Phone = status.User.Phone
		}
		m.log.Info("telegram session ready",
			zap.Int64("user_id", status.User.GetID()),
			zap.String("username", status.User.Username))
	} else {
		m.self = nil
		m.state = StateUnauthorized
	}
	self := m.self
	onReady = m.OnReady
	m.mu.Unlock()

	// Caching who this account is lets the accounts list name it without a
	// live connection, which is exactly the situation where an operator most
	// wants to know which login is broken.
	if self != nil && accountID != "" {
		if err := m.db.SetAccountIdentity(ctx, accountID, self.GetID(), self.Username, self.Phone); err != nil {
			m.log.Warn("could not cache the telegram account identity", zap.Error(err))
		}
	}

	if becameReady && onReady != nil {
		go onReady()
	}
}

// Configure stores new app credentials on this account and reconnects with
// them.
func (m *Manager) Configure(ctx context.Context, appID int, appHash string) error {
	if appID == 0 || appHash == "" {
		return errors.New("app id and app hash are both required")
	}

	m.mu.Lock()
	acct := m.acct
	m.acct.AppID, m.acct.AppHash = appID, appHash
	m.mu.Unlock()

	if acct.ID != "" {
		if err := m.db.UpdateAccountSettings(ctx, acct.ID, acct.Label, appID, appHash, acct.Enabled); err != nil {
			return err
		}
	}
	// The primary account's credentials are also the ones the settings page and
	// the setup wizard show, so they stay mirrored into the runtime snapshot.
	if acct.IsPrimary {
		if err := m.db.SetSettings(ctx, map[string]string{
			database.SettingTGAppID:   strconv.Itoa(appID),
			database.SettingTGAppHash: appHash,
		}); err != nil {
			return err
		}
		settings := m.cfg.RuntimeSettings()
		settings.AppID, settings.AppHash = appID, appHash
		m.cfg.SetRuntimeSettings(settings)
	}
	return m.start(ctx, appID, appHash)
}

// Reload rebuilds the Telegram connection pool with the current runtime
// settings while keeping the existing session and account login.
func (m *Manager) Reload(ctx context.Context) error {
	if !m.Ready() {
		return nil
	}
	appID, appHash, err := m.Credentials(ctx)
	if err != nil {
		return err
	}
	return m.start(ctx, appID, appHash)
}

// Stop closes the pool and ends the run loop. It is safe to call when nothing
// is running.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel, done, p := m.cancel, m.done, m.pool
	m.cancel, m.done, m.pool, m.client, m.self = nil, nil, nil, nil, nil
	m.mu.Unlock()

	if p != nil {
		_ = p.Close()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			m.log.Warn("telegram run loop did not stop in time")
		}
	}
}

// API returns a pooled client for an authorized session.
func (m *Manager) API(ctx context.Context) (*tg.Client, error) {
	m.mu.RLock()
	state, p := m.state, m.pool
	m.mu.RUnlock()

	if state != StateReady || p == nil {
		return nil, ErrNotReady
	}
	return p.Default(ctx), nil
}

// APIForDC returns a pooled client bound to a specific datacenter, used when a
// document lives outside the session's home DC.
func (m *Manager) APIForDC(ctx context.Context, dc int) (*tg.Client, error) {
	m.mu.RLock()
	state, p := m.state, m.pool
	m.mu.RUnlock()

	if state != StateReady || p == nil {
		return nil, ErrNotReady
	}
	if dc <= 0 {
		return p.Default(ctx), nil
	}
	return p.Client(ctx, dc), nil
}

// Raw exposes the underlying client for auth calls, which must run on the main
// connection rather than a pooled one.
func (m *Manager) Raw() (*telegram.Client, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.client == nil {
		return nil, ErrNotConfigured
	}
	return m.client, nil
}

// Ready reports whether uploads and downloads can run.
func (m *Manager) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == StateReady
}

// Status snapshots the connection for the API.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s := Status{
		State:            m.state,
		AwaitingCode:     m.loginCodeHash != "" && !m.awaitingPass,
		AwaitingPassword: m.awaitingPass,
		Phone:            maskPhone(m.loginPhone),
	}
	if m.runErr != nil {
		s.Error = m.runErr.Error()
	}
	if m.self != nil {
		s.UserID = m.self.GetID()
		s.Username = m.self.Username
		s.FirstName = m.self.FirstName
		s.Premium = m.self.Premium
		if m.self.Phone != "" {
			s.Phone = maskPhone(m.self.Phone)
		}
	}
	if m.client != nil && m.state == StateReady {
		s.DC = m.client.Config().ThisDC
	}
	s.CooldownMs = m.Cooldown().Milliseconds()
	return s
}

// Self returns the signed-in account, which the uploader uses to decide whether
// the premium file size limit applies.
func (m *Manager) Self() *tg.User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.self
}

func (m *Manager) setState(s State, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.runErr = s, err
}

// maskPhone keeps enough of a number to recognise the account without printing
// it in full to every browser tab that polls the status endpoint.
func maskPhone(p string) string {
	if len(p) <= 4 {
		return p
	}
	return "•••••" + p[len(p)-4:]
}

// Version is reported to Telegram as the client app version and shown in the
// UI. main overwrites it with the value stamped in at build time.
var Version = "dev"
