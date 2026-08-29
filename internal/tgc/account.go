package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gotd/td/session"

	"github.com/dibin/tdrive/internal/database"
)

const maxSessionSize = 1 << 20

// ErrInvalidSession marks a user-supplied account package whose session data
// cannot be consumed by gotd.
var ErrInvalidSession = errors.New("tgc: invalid telegram session")

// ExportSession returns the gotd session file for the currently signed-in
// Telegram account. The session is a credential, so this method deliberately
// refuses to export an account that is not ready.
func (m *Manager) ExportSession(ctx context.Context) ([]byte, error) {
	m.mu.RLock()
	state := m.state
	m.mu.RUnlock()
	if state != StateReady {
		return nil, ErrNotReady
	}

	data, err := os.ReadFile(m.cfg.Telegram.SessionFile)
	if os.IsNotExist(err) {
		return nil, ErrNotReady
	}
	if err != nil {
		return nil, fmt.Errorf("read telegram session: %w", err)
	}
	if err := validateSession(ctx, data); err != nil {
		return nil, fmt.Errorf("validate telegram session: %w", err)
	}
	return data, nil
}

// ImportSession replaces the local Telegram session and reconnects with the
// supplied app credentials. The session is validated before the running
// client is stopped, and is written atomically so a failed upload cannot leave
// a truncated credential file behind.
func (m *Manager) ImportSession(ctx context.Context, appID int, appHash string, data []byte) error {
	if appID <= 0 {
		return errors.New("telegram app id must be positive")
	}
	appHash = strings.TrimSpace(appHash)
	if appHash == "" {
		return errors.New("telegram app hash is required")
	}
	if err := validateSession(ctx, data); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSession, err)
	}

	m.Stop()
	m.CancelLogin()
	if err := replaceSessionFile(m.cfg.Telegram.SessionFile, data); err != nil {
		m.setState(StateError, err)
		return err
	}
	if err := m.db.SetSetting(ctx, database.SettingTGAppID, fmt.Sprint(appID)); err != nil {
		err = fmt.Errorf("store telegram app id: %w", err)
		m.setState(StateError, err)
		return err
	}
	if err := m.db.SetSetting(ctx, database.SettingTGAppHash, appHash); err != nil {
		err = fmt.Errorf("store telegram app hash: %w", err)
		m.setState(StateError, err)
		return err
	}
	settings := m.cfg.RuntimeSettings()
	settings.AppID, settings.AppHash = appID, appHash
	m.cfg.SetRuntimeSettings(settings)
	return m.start(ctx, appID, appHash)
}

func validateSession(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return errors.New("session is empty")
	}
	if len(data) > maxSessionSize {
		return fmt.Errorf("session is too large (maximum %d bytes)", maxSessionSize)
	}

	storage := &session.StorageMemory{}
	if err := storage.StoreSession(ctx, data); err != nil {
		return err
	}
	loader := session.Loader{Storage: storage}
	parsed, err := loader.Load(ctx)
	if err != nil {
		return err
	}
	if len(parsed.AuthKey) == 0 {
		return errors.New("session has no authorization key")
	}
	return nil
}

func replaceSessionFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".session-import-*")
	if err != nil {
		return fmt.Errorf("create temporary session: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect temporary session: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary session: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace telegram session: %w", err)
	}
	return nil
}
