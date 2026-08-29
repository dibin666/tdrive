// Package tgc owns the Telegram connection: its lifecycle, its connection pool
// and the login flow that the WebUI drives.
//
// gotd's Client.Run blocks for as long as the connection should live, which
// suits a CLI but not a server that has to answer HTTP requests the whole time.
// Manager therefore runs it on its own goroutine and publishes a snapshot of
// the connection state that handlers can read without blocking.
package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
}

// Manager owns the single Telegram connection shared by the whole process.
type Manager struct {
	cfg *config.Config
	db  *database.DB
	log *zap.Logger

	mu     sync.RWMutex
	state  State
	runErr error
	client *telegram.Client
	pool   Pool
	self   *tg.User

	cancel context.CancelFunc
	done   chan struct{}

	// Login flow state. Telegram hands back a phone_code_hash with the sent
	// code and expects it echoed on sign-in, so it has to survive between two
	// unrelated HTTP requests.
	loginPhone    string
	loginCodeHash string
	awaitingPass  bool
}

// New builds a manager. It does not connect; call Start.
func New(cfg *config.Config, db *database.DB, log *zap.Logger) *Manager {
	return &Manager{cfg: cfg, db: db, log: log, state: StateUnconfigured}
}

// Credentials resolves the api_id/api_hash pair, preferring the environment so
// that a container can be configured without touching the database, and
// falling back to whatever the setup wizard stored.
func (m *Manager) Credentials(ctx context.Context) (int, string, error) {
	appID, appHash := m.cfg.Telegram.AppID, m.cfg.Telegram.AppHash
	if appID == 0 {
		if v := m.db.SettingOr(ctx, database.SettingTGAppID, ""); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return 0, "", fmt.Errorf("stored app id %q is not a number: %w", v, err)
			}
			appID = n
		}
	}
	if appHash == "" {
		appHash = m.db.SettingOr(ctx, database.SettingTGAppHash, "")
	}
	if appID == 0 || appHash == "" {
		return 0, "", ErrNotConfigured
	}
	return appID, appHash, nil
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

	if err := os.MkdirAll(filepath.Dir(m.cfg.Telegram.SessionFile), 0o750); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}

	// Rate limiting smooths bursts so Telegram is less likely to answer with
	// FLOOD_WAIT at all; the waiter then absorbs the ones that still happen,
	// which is what the reference implementation does by hand around every
	// send_message call.
	middlewares := []telegram.Middleware{
		floodwait.NewSimpleWaiter().WithMaxRetries(5).WithMaxWait(5 * time.Minute),
		ratelimit.New(rate.Every(m.cfg.Telegram.RateLimit), m.cfg.Telegram.RateBurst),
	}

	client := telegram.NewClient(appID, appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: m.cfg.Telegram.SessionFile},
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
	m.pool = NewPool(client, m.cfg.Telegram.PoolSize, middlewares...)
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
	defer m.mu.Unlock()
	if status.Authorized {
		m.self = status.User
		m.state = StateReady
		m.loginPhone, m.loginCodeHash, m.awaitingPass = "", "", false
		m.log.Info("telegram session ready",
			zap.Int64("user_id", status.User.GetID()),
			zap.String("username", status.User.Username))
	} else {
		m.self = nil
		m.state = StateUnauthorized
	}
}

// Configure stores new app credentials and reconnects with them.
func (m *Manager) Configure(ctx context.Context, appID int, appHash string) error {
	if appID == 0 || appHash == "" {
		return errors.New("app id and app hash are both required")
	}
	if err := m.db.SetSetting(ctx, database.SettingTGAppID, strconv.Itoa(appID)); err != nil {
		return err
	}
	if err := m.db.SetSetting(ctx, database.SettingTGAppHash, appHash); err != nil {
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
