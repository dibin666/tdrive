package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	goPlugin "github.com/hashicorp/go-plugin"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
	"github.com/dibin/tdrive/internal/tgc"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

const (
	inspectionLifetime = 10 * time.Minute
	pluginCallTimeout  = 30 * time.Second
	pluginDataDirEnv   = "TDRIVE_PLUGIN_DATA_DIR"
)

// Inspection is the one-time manifest review shown by the WebUI before an
// installation is confirmed. The manifest digest binds the later download to
// the exact document reviewed here, and the manifest in turn fixes the binary
// digest, so confirming is a decision about known bytes.
type Inspection struct {
	ID             string                `json:"inspectionId"`
	Manifest       tdriveplugin.Manifest `json:"manifest"`
	ManifestURL    string                `json:"manifestUrl"`
	ManifestDigest string                `json:"manifestDigest"`
	Platform       string                `json:"platform"`
	BinaryURL      string                `json:"binaryUrl"`
	BinaryDigest   string                `json:"binaryDigest"`
	Compatible     bool                  `json:"compatible"`
	IsUpdate       bool                  `json:"isUpdate"`
	CurrentVersion string                `json:"currentVersion,omitempty"`
	Warning        string                `json:"warning,omitempty"`
	ExpiresAt      time.Time             `json:"expiresAt"`
}

// StoreIndex is the intentionally boring JSON format consumed by the plugin
// store UI. A store only discovers plugin metadata; installation still goes
// through the same inspect-and-confirm flow as a manually entered URL.
type StoreIndex struct {
	UpdatedAt time.Time     `json:"updatedAt"`
	Plugins   []StorePlugin `json:"plugins"`
}

type StorePlugin struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Version        string   `json:"version"`
	Author         string   `json:"author"`
	RepositoryURL  string   `json:"repositoryUrl"`
	ManifestURL    string   `json:"manifestUrl"`
	ManifestDigest string   `json:"manifestDigest"`
	Documentation  string   `json:"documentationUrl,omitempty"`
	License        string   `json:"license"`
	Tags           []string `json:"tags,omitempty"`
}

// PluginStatus is safe to return to the WebUI. The raw binary path and
// manifest JSON remain host-only fields.
type PluginStatus struct {
	ID             string                `json:"id"`
	Manifest       tdriveplugin.Manifest `json:"manifest"`
	Enabled        bool                  `json:"enabled"`
	Status         string                `json:"status"`
	Source         string                `json:"source"`
	ManifestURL    string                `json:"manifestUrl,omitempty"`
	ManifestDigest string                `json:"manifestDigest"`
	BinaryDigest   string                `json:"binaryDigest"`
	Error          string                `json:"error,omitempty"`
	InstalledAt    time.Time             `json:"installedAt"`
	UpdatedAt      time.Time             `json:"updatedAt"`
}

type activePlugin struct {
	record   database.PluginRecord
	manifest tdriveplugin.Manifest
	process  *goPlugin.Client
	protocol goPlugin.ClientProtocol
	client   pluginRuntimeClient
	// failed keeps the public route declared while excluding an unavailable
	// child from core hooks and event delivery. A dead plugin must not make
	// unrelated file operations fail while it is being restarted.
	failed bool
}

// pluginRuntimeClient is the part of the SDK client used after a plugin has
// been started. Keeping the small interface here makes the failure/recovery
// path testable without starting a child process, and more importantly keeps
// the active route alive while a dead child is being replaced.
type pluginRuntimeClient interface {
	Before(context.Context, tdriveplugin.Operation) (tdriveplugin.OperationResult, error)
	After(context.Context, tdriveplugin.Operation) error
	OnEvent(context.Context, tdriveplugin.Event) error
	HandleHTTP(context.Context, tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error)
	Shutdown(context.Context) error
}

type pendingInspection struct {
	inspection Inspection
}

// Manager owns plugin binaries and child processes. It is safe to construct
// even when the database contains no plugins: no source directory is scanned,
// no child process is started, and no event subscriber is created in that
// case.
type Manager struct {
	cfg    *config.Config
	db     *database.DB
	auth   *auth.Service
	drive  *drive.Service
	tg     TelegramStatus
	broker *events.Broker
	log    *zap.Logger

	fetcher releaseFetcher

	installMu    sync.Mutex
	mu           sync.RWMutex
	hooksEnabled atomic.Bool
	active       map[string]*activePlugin
	recovering   map[string]bool
	recoveryCtx  context.Context
	recoveryStop context.CancelFunc
	recoveryWG   sync.WaitGroup
	inspections  map[string]pendingInspection
	eventsStop   context.CancelFunc
	closed       bool
}

// TelegramStatus is all a plugin may learn about the Telegram side: whether the
// drive's account is connected. A deployment holds one account per api_id pair,
// so this reports the primary one rather than exposing the whole cluster to
// plugin code.
type TelegramStatus interface {
	Status() tgc.Status
}

// New creates a manager without touching the plugin directory. The fetcher is
// only an HTTP client; nothing is downloaded until Inspect or Install is
// called.
func New(
	cfg *config.Config,
	db *database.DB,
	authSvc *auth.Service,
	driveSvc *drive.Service,
	tgm TelegramStatus,
	broker *events.Broker,
	log *zap.Logger,
) *Manager {
	if log == nil {
		log = zap.NewNop()
	}
	recoveryCtx, recoveryStop := context.WithCancel(context.Background())
	return &Manager{
		cfg:          cfg,
		db:           db,
		auth:         authSvc,
		drive:        driveSvc,
		tg:           tgm,
		broker:       broker,
		log:          log,
		fetcher:      newHTTPFetcher(cfg.Plugins.MaxBinaryBytes),
		active:       make(map[string]*activePlugin),
		recovering:   make(map[string]bool),
		recoveryCtx:  recoveryCtx,
		recoveryStop: recoveryStop,
		inspections:  make(map[string]pendingInspection),
	}
}

// SetFetcher is intended for package tests, which cannot reach a public HTTPS
// host.
func (manager *Manager) SetFetcher(fetcher releaseFetcher) {
	manager.mu.Lock()
	manager.fetcher = fetcher
	manager.mu.Unlock()
}

// Start loads only enabled records and starts their already-installed
// binaries. A broken plugin is isolated and recorded as an error; it does not
// prevent the drive or WebUI from starting.
func (manager *Manager) Start(ctx context.Context) error {
	records, err := manager.db.ListEnabledPlugins(ctx)
	if err != nil {
		return fmt.Errorf("list enabled plugins: %w", err)
	}
	for _, record := range records {
		loaded, err := manager.startRuntime(ctx, record)
		if err != nil {
			manager.log.Error("plugin failed to start", zap.String("plugin", record.ID), zap.Error(err))
			if _, stateErr := manager.db.UpdatePluginState(ctx, record.ID, true, database.PluginStatusError, err.Error()); stateErr != nil {
				manager.log.Warn("could not record plugin startup failure", zap.String("plugin", record.ID), zap.Error(stateErr))
			}
			if placeholder := unavailableRuntime(record, err); placeholder != nil {
				manager.mu.Lock()
				if !manager.closed {
					manager.active[record.ID] = placeholder
				}
				manager.mu.Unlock()
				manager.scheduleRestart(placeholder, err)
			}
			continue
		}
		manager.mu.Lock()
		manager.active[record.ID] = loaded
		manager.mu.Unlock()
		manager.refreshDriveHooks()
		if _, stateErr := manager.db.UpdatePluginState(ctx, record.ID, true, database.PluginStatusActive, ""); stateErr != nil {
			manager.log.Warn("could not record active plugin state", zap.String("plugin", record.ID), zap.Error(stateErr))
		}
	}
	manager.startEventBridge(ctx)
	return nil
}

// Close gracefully stops the plugin children. It is deliberately idempotent so
// shutdown paths can defer it.
func (manager *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	stopEvents := manager.eventsStop
	manager.eventsStop = nil
	plugins := make([]*activePlugin, 0, len(manager.active))
	for id, plugin := range manager.active {
		plugins = append(plugins, plugin)
		delete(manager.active, id)
	}
	manager.mu.Unlock()
	manager.refreshDriveHooks()
	if manager.recoveryStop != nil {
		manager.recoveryStop()
	}

	if stopEvents != nil {
		stopEvents()
	}
	for _, plugin := range plugins {
		manager.stopRuntime(ctx, plugin)
	}
	manager.waitRecoveries(ctx)
	return nil
}

// HasHooks is used by drive.Service to preserve a nil fast path when no
// active plugin implements the operation hook bridge.
func (manager *Manager) HasHooks() bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.hasReadyPluginLocked()
}

func (manager *Manager) hasReadyPluginLocked() bool {
	for _, active := range manager.active {
		if active != nil && !active.failed {
			return true
		}
	}
	return false
}

// Before chains active plugins in installation order. A plugin may reject the
// operation or replace its JSON payload for the next plugin and the core.
func (manager *Manager) Before(ctx context.Context, operation tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	if tdriveplugin.IsHostCall(ctx) {
		return tdriveplugin.OperationResult{Allowed: true, Payload: operation.Payload}, nil
	}
	plugins := manager.snapshotActive()
	result := tdriveplugin.OperationResult{Allowed: true, Payload: operation.Payload}
	for _, active := range plugins {
		operation.Payload = result.Payload
		callCtx, cancel := pluginCallContext(ctx)
		pluginResult, err := active.client.Before(callCtx, operation)
		cancel()
		if err != nil {
			manager.handlePluginFailure(active.record.ID, err)
			return tdriveplugin.OperationResult{}, fmt.Errorf("plugin %q before hook: %w", active.record.ID, err)
		}
		if !pluginResult.Allowed {
			if pluginResult.Error == "" {
				pluginResult.Error = "operation rejected by plugin " + active.record.ID
			}
			return pluginResult, nil
		}
		result = pluginResult
		if len(result.Payload) == 0 {
			result.Payload = operation.Payload
		}
	}
	return result, nil
}

// After is best-effort because the core operation has already completed. A
// failing plugin is stopped so subsequent requests do not block on a dead RPC
// connection.
func (manager *Manager) After(ctx context.Context, operation tdriveplugin.Operation) {
	for _, active := range manager.snapshotActive() {
		callCtx, cancel := pluginCallContext(ctx)
		err := active.client.After(callCtx, operation)
		cancel()
		if err != nil {
			manager.log.Warn("plugin after hook failed", zap.String("plugin", active.record.ID), zap.Error(err))
			manager.handlePluginFailure(active.record.ID, err)
		}
	}
}

// Inspect downloads and validates a plugin manifest and stores a one-time
// inspection token. Nothing is executed here and no plugin binary is fetched:
// the review shown to the administrator is built purely from the manifest.
// The token is consumed by Install, so a confirmation cannot be replayed
// against a different manifest.
func (manager *Manager) Inspect(ctx context.Context, manifestURL string, expectedDigest ...string) (Inspection, error) {
	if _, err := ValidateDownloadURL(manifestURL); err != nil {
		return Inspection{}, err
	}
	manager.mu.RLock()
	fetcher := manager.fetcher
	manager.mu.RUnlock()
	if fetcher == nil {
		return Inspection{}, errors.New("plugin installer is not configured")
	}
	manifest, manifestDigest, err := fetcher.Manifest(ctx, manifestURL)
	if err != nil {
		return Inspection{}, err
	}
	if len(expectedDigest) > 0 && expectedDigest[0] != "" && !strings.EqualFold(expectedDigest[0], manifestDigest) {
		return Inspection{}, fmt.Errorf("plugin manifest digest changed: expected %s, got %s", expectedDigest[0], manifestDigest)
	}
	if manifest.APIVersion != tdriveplugin.APIVersion {
		return Inspection{}, fmt.Errorf("plugin API version %d is not supported; host uses %d", manifest.APIVersion, tdriveplugin.APIVersion)
	}
	artifact, err := manifest.ArtifactFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Inspection{}, err
	}
	inspection := Inspection{
		ID:             database.NewID(),
		Manifest:       manifest,
		ManifestURL:    manifestURL,
		ManifestDigest: manifestDigest,
		Platform:       tdriveplugin.HostPlatform(),
		BinaryURL:      artifact.URL,
		BinaryDigest:   artifact.SHA256,
		Compatible:     true,
		Warning:        "全信任插件可调用和修改 tdrive 暴露的全部功能，并以 tdrive 进程的权限运行。",
		ExpiresAt:      time.Now().Add(inspectionLifetime),
	}
	if current, err := manager.db.PluginByID(ctx, manifest.ID); err == nil {
		inspection.IsUpdate = true
		inspection.CurrentVersion = current.Version
	}
	manager.mu.Lock()
	manager.inspections[inspection.ID] = pendingInspection{inspection: inspection}
	manager.mu.Unlock()
	return inspection, nil
}

// Install consumes an inspection token and activates the resulting plugin.
// The caller must have already performed the one UI confirmation; this method
// intentionally accepts no second permission or capability choice.
func (manager *Manager) Install(ctx context.Context, inspectionID string) (PluginStatus, error) {
	manager.installMu.Lock()
	defer manager.installMu.Unlock()

	inspection, err := manager.consumeInspection(inspectionID)
	if err != nil {
		return PluginStatus{}, err
	}
	if !inspection.Compatible || time.Now().After(inspection.ExpiresAt) {
		return PluginStatus{}, errors.New("plugin inspection is no longer valid")
	}

	if err := os.MkdirAll(manager.cfg.Plugins.Dir, 0o750); err != nil {
		return PluginStatus{}, fmt.Errorf("create plugin directory: %w", err)
	}
	stagingDir := filepath.Join(manager.cfg.Plugins.Dir, ".staging", inspection.Manifest.ID+"-"+database.NewID())
	stagingBinary := filepath.Join(stagingDir, executableName("plugin", runtime.GOOS))
	defer os.RemoveAll(stagingDir)

	manager.mu.RLock()
	fetcher := manager.fetcher
	manager.mu.RUnlock()
	if fetcher == nil {
		return PluginStatus{}, errors.New("plugin installer is not configured")
	}
	// The digest comes from the manifest the administrator confirmed, so the
	// download either produces exactly those bytes or produces nothing.
	binaryDigest, err := fetcher.Download(ctx,
		tdriveplugin.Artifact{URL: inspection.BinaryURL, SHA256: inspection.BinaryDigest}, stagingBinary)
	if err != nil {
		return PluginStatus{}, err
	}

	manifestJSON, err := json.Marshal(inspection.Manifest)
	if err != nil {
		return PluginStatus{}, fmt.Errorf("encode plugin manifest: %w", err)
	}
	finalPath := filepath.Join(manager.cfg.Plugins.Dir, executableName(inspection.Manifest.ID, runtime.GOOS))
	oldRecord, oldRecordErr := manager.db.PluginByID(ctx, inspection.Manifest.ID)
	if oldRecordErr != nil && !errors.Is(oldRecordErr, database.ErrNotFound) {
		return PluginStatus{}, fmt.Errorf("read existing plugin metadata: %w", oldRecordErr)
	}
	if oldRecordErr == nil {
		oldRecord = manager.normalizePluginRecord(oldRecord)
	}
	oldActive := manager.takeActive(inspection.Manifest.ID)
	if oldActive != nil {
		manager.stopRuntime(ctx, oldActive)
	}
	manager.refreshDriveHooks()

	backupPath := finalPath + ".old-" + database.NewID()
	hadOldBinary := false
	if _, statErr := os.Stat(finalPath); statErr == nil {
		if err := os.Rename(finalPath, backupPath); err != nil {
			if oldActive != nil {
				manager.restoreRuntime(ctx, inspection.Manifest.ID, oldRecord)
			}
			return PluginStatus{}, fmt.Errorf("stage existing plugin binary: %w", err)
		}
		hadOldBinary = true
	} else if !os.IsNotExist(statErr) {
		if oldActive != nil {
			manager.restoreRuntime(ctx, inspection.Manifest.ID, oldRecord)
		}
		return PluginStatus{}, fmt.Errorf("inspect existing plugin binary: %w", statErr)
	}
	metadataInstalled := false
	restoreOld := func() {
		_ = os.Remove(finalPath)
		if hadOldBinary {
			_ = os.Rename(backupPath, finalPath)
		}
		if metadataInstalled {
			if oldRecordErr == nil {
				if restoreErr := manager.db.UpsertPlugin(ctx, oldRecord); restoreErr != nil {
					manager.log.Warn("could not restore previous plugin metadata", zap.String("plugin", inspection.Manifest.ID), zap.Error(restoreErr))
				}
			} else if deleteErr := manager.db.DeletePlugin(ctx, inspection.Manifest.ID); deleteErr != nil && !errors.Is(deleteErr, database.ErrNotFound) {
				manager.log.Warn("could not remove failed plugin metadata", zap.String("plugin", inspection.Manifest.ID), zap.Error(deleteErr))
			}
		}
		if oldActive != nil {
			manager.restoreRuntime(ctx, inspection.Manifest.ID, oldRecord)
		}
	}
	if err := os.Rename(stagingBinary, finalPath); err != nil {
		restoreOld()
		return PluginStatus{}, fmt.Errorf("install plugin binary: %w", err)
	}

	now := time.Now()
	record := database.PluginRecord{
		ID:             inspection.Manifest.ID,
		Name:           inspection.Manifest.Name,
		Version:        inspection.Manifest.Version,
		Author:         inspection.Manifest.Author,
		Enabled:        true,
		Status:         database.PluginStatusActive,
		Source:         "release",
		ManifestURL:    inspection.ManifestURL,
		ManifestDigest: inspection.ManifestDigest,
		BinaryDigest:   binaryDigest,
		BinaryPath:     finalPath,
		ManifestJSON:   string(manifestJSON),
		InstalledAt:    now,
		UpdatedAt:      now,
	}
	record.Status = database.PluginStatusStopped
	if err := manager.db.UpsertPlugin(ctx, record); err != nil {
		restoreOld()
		return PluginStatus{}, err
	}
	metadataInstalled = true
	loaded, err := manager.startRuntime(ctx, record)
	if err != nil {
		restoreOld()
		return PluginStatus{}, fmt.Errorf("start installed plugin: %w", err)
	}
	if _, err := manager.db.UpdatePluginState(ctx, record.ID, true, database.PluginStatusActive, ""); err != nil {
		manager.stopRuntime(ctx, loaded)
		restoreOld()
		return PluginStatus{}, err
	}
	record.Status = database.PluginStatusActive
	manager.mu.Lock()
	manager.active[record.ID] = loaded
	manager.mu.Unlock()
	manager.refreshDriveHooks()
	if hadOldBinary {
		_ = os.Remove(backupPath)
	}
	// An upgrade whose predecessor was installed under a different file name
	// leaves that file behind, because the backup dance above only knows about
	// finalPath. This happens when the naming rule itself changed — a Windows
	// plugin installed before the .exe suffix was added.
	if oldRecordErr == nil && oldRecord.BinaryPath != "" && oldRecord.BinaryPath != finalPath {
		if err := os.Remove(oldRecord.BinaryPath); err != nil && !os.IsNotExist(err) {
			manager.log.Warn("could not remove the previously installed plugin binary",
				zap.String("plugin", record.ID), zap.String("path", oldRecord.BinaryPath), zap.Error(err))
		}
	}
	manager.startEventBridge(ctx)
	return manager.toStatus(record), nil
}

func (manager *Manager) consumeInspection(id string) (Inspection, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	pending, ok := manager.inspections[id]
	if ok {
		delete(manager.inspections, id)
	}
	if !ok {
		return Inspection{}, errors.New("plugin inspection was not found or already used")
	}
	return pending.inspection, nil
}

// List returns all installed records, including disabled and failed plugins.
func (manager *Manager) List(ctx context.Context) ([]PluginStatus, error) {
	records, err := manager.db.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	statuses := make([]PluginStatus, 0, len(records))
	for _, record := range records {
		statuses = append(statuses, manager.toStatus(record))
	}
	return statuses, nil
}

// SetEnabled starts or stops one installed plugin.
func (manager *Manager) SetEnabled(ctx context.Context, id string, enabled bool) (PluginStatus, error) {
	record, err := manager.db.PluginByID(ctx, id)
	if err != nil {
		return PluginStatus{}, err
	}
	if !enabled {
		active := manager.takeActive(id)
		if active != nil {
			manager.stopRuntime(ctx, active)
		}
		manager.refreshDriveHooks()
		manager.stopEventBridgeIfEmpty()
		updated, err := manager.db.UpdatePluginState(ctx, id, false, database.PluginStatusDisabled, "")
		if err != nil {
			return PluginStatus{}, err
		}
		manager.startEventBridge(ctx)
		return manager.toStatus(updated), nil
	}

	if active := manager.getActive(id); active != nil {
		updated, err := manager.db.UpdatePluginState(ctx, id, true, database.PluginStatusActive, "")
		if err != nil {
			return PluginStatus{}, err
		}
		if active.failed {
			manager.scheduleRestart(active, errors.New("管理员请求重新启动插件"))
			if current, readErr := manager.db.PluginByID(context.Background(), id); readErr == nil {
				return manager.toStatus(current), nil
			}
		}
		return manager.toStatus(updated), nil
	}
	loaded, err := manager.startRuntime(ctx, record)
	if err != nil {
		_, _ = manager.db.UpdatePluginState(ctx, id, true, database.PluginStatusError, err.Error())
		if placeholder := unavailableRuntime(record, err); placeholder != nil {
			manager.mu.Lock()
			if !manager.closed {
				manager.active[id] = placeholder
			}
			manager.mu.Unlock()
			manager.scheduleRestart(placeholder, err)
		}
		return PluginStatus{}, err
	}
	manager.mu.Lock()
	manager.active[id] = loaded
	manager.mu.Unlock()
	manager.refreshDriveHooks()
	updated, err := manager.db.UpdatePluginState(ctx, id, true, database.PluginStatusActive, "")
	if err != nil {
		manager.handlePluginFailure(id, err)
		return PluginStatus{}, err
	}
	manager.startEventBridge(ctx)
	return manager.toStatus(updated), nil
}

// Uninstall stops a plugin, removes its binary and deletes its metadata. The
// database row is deleted last so a failed filesystem operation leaves a
// recoverable record rather than a half-uninstalled state.
func (manager *Manager) Uninstall(ctx context.Context, id string) error {
	record, err := manager.db.PluginByID(ctx, id)
	if err != nil {
		return err
	}
	record = manager.normalizePluginRecord(record)
	active := manager.takeActive(id)
	if active != nil {
		manager.stopRuntime(ctx, active)
	}
	manager.refreshDriveHooks()
	backupPath := ""
	if record.BinaryPath != "" {
		backupPath = record.BinaryPath + ".uninstall-" + database.NewID()
		if err := os.Rename(record.BinaryPath, backupPath); err != nil && !os.IsNotExist(err) {
			if active != nil {
				manager.restoreRuntime(ctx, id, record)
			}
			return fmt.Errorf("stage plugin binary for removal: %w", err)
		}
	}
	if err := manager.db.DeletePlugin(ctx, id); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, record.BinaryPath)
		}
		if active != nil {
			manager.restoreRuntime(ctx, id, record)
		}
		return err
	}
	if backupPath != "" {
		_ = os.Remove(backupPath)
	}
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
	manager.startEventBridge(ctx)
	return nil
}

func (manager *Manager) restoreRuntime(ctx context.Context, id string, record database.PluginRecord) {
	restarted, err := manager.startRuntime(ctx, record)
	if err != nil {
		manager.log.Warn("could not restore plugin after a failed management action", zap.String("plugin", id), zap.Error(err))
		return
	}
	manager.mu.Lock()
	manager.active[id] = restarted
	manager.mu.Unlock()
	manager.refreshDriveHooks()
}

// Settings reads opaque JSON state for a plugin.
func (manager *Manager) Settings(ctx context.Context, id string) (json.RawMessage, error) {
	if _, err := manager.db.PluginByID(ctx, id); err != nil {
		return nil, err
	}
	value, err := manager.db.PluginData(ctx, id, "settings")
	if errors.Is(err, database.ErrNotFound) {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(value), err
}

// UpdateSettings stores opaque JSON state for a plugin.
func (manager *Manager) UpdateSettings(ctx context.Context, id string, value json.RawMessage) error {
	if _, err := manager.db.PluginByID(ctx, id); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return fmt.Errorf("plugin settings must be a JSON object: %w", err)
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return err
	}
	return manager.db.SetPluginData(ctx, id, "settings", canonical)
}

func (manager *Manager) startRuntime(ctx context.Context, record database.PluginRecord) (*activePlugin, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	record = manager.normalizePluginRecord(record)
	if record.BinaryPath == "" {
		return nil, errors.New("plugin binary path is empty")
	}
	if record.ManifestJSON == "" {
		return nil, errors.New("plugin manifest is empty")
	}
	var manifest tdriveplugin.Manifest
	if err := json.Unmarshal([]byte(record.ManifestJSON), &manifest); err != nil {
		return nil, fmt.Errorf("decode stored plugin manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if manifest.APIVersion != tdriveplugin.APIVersion {
		return nil, fmt.Errorf("plugin API version %d is not supported", manifest.APIVersion)
	}
	if manifest.ID != record.ID {
		return nil, fmt.Errorf("plugin manifest id %q does not match installed id %q", manifest.ID, record.ID)
	}
	if _, err := os.Stat(record.BinaryPath); err != nil {
		return nil, fmt.Errorf("stat plugin binary: %w", err)
	}
	checksum, err := hex.DecodeString(record.BinaryDigest)
	if err != nil || len(checksum) != sha256.Size {
		return nil, errors.New("stored plugin binary digest is invalid")
	}
	// The plugin executable may be replaced during an upgrade. Its private
	// state must therefore live under the deployment data volume, not be
	// inferred from whichever copy of the executable happened to be launched.
	// The child gets this path explicitly so a plugin update, restart, or
	// recovery cannot silently move its own downloaded tools to another tree.
	dataDir := manager.pluginDataDir(record)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create plugin data directory: %w", err)
	}

	command := exec.Command(record.BinaryPath)
	command.Env = pluginEnvironment(dataDir)
	process := goPlugin.NewClient(&goPlugin.ClientConfig{
		HandshakeConfig: tdriveplugin.HandshakeConfig,
		Plugins: goPlugin.PluginSet{
			tdriveplugin.PluginName: &tdriveplugin.RPCPlugin{},
		},
		Cmd: command,
		// command.Env already contains the complete parent environment plus the
		// stable plugin-data override. go-plugin otherwise appends os.Environ a
		// second time, which would create duplicate data-dir keys.
		SkipHostEnv:  true,
		SecureConfig: &goPlugin.SecureConfig{Checksum: checksum, Hash: sha256.New()},
		Logger:       hclog.NewNullLogger(),
	})
	protocol, err := process.Client()
	if err != nil {
		process.Kill()
		return nil, fmt.Errorf("connect plugin process: %w", err)
	}
	dispensed, err := protocol.Dispense(tdriveplugin.PluginName)
	if err != nil {
		process.Kill()
		return nil, fmt.Errorf("dispense plugin: %w", err)
	}
	client, ok := dispensed.(*tdriveplugin.Client)
	if !ok {
		process.Kill()
		return nil, errors.New("plugin returned an unexpected RPC client")
	}
	callCtx, cancel := pluginCallContext(ctx)
	remoteManifest, err := client.Manifest(callCtx)
	cancel()
	if err != nil {
		process.Kill()
		return nil, fmt.Errorf("read plugin manifest: %w", err)
	}
	if !manifestsMatch(remoteManifest, manifest) {
		process.Kill()
		return nil, errors.New("compiled plugin manifest does not match installed manifest")
	}
	host := &managerHost{manager: manager, pluginID: record.ID}
	callCtx, cancel = pluginCallContext(ctx)
	err = client.AttachHost(callCtx, host)
	cancel()
	if err != nil {
		process.Kill()
		return nil, fmt.Errorf("initialize plugin: %w", err)
	}
	return &activePlugin{record: record, manifest: manifest, process: process, protocol: protocol, client: client}, nil
}

func (manager *Manager) normalizePluginRecord(record database.PluginRecord) database.PluginRecord {
	if record.BinaryPath == "" || filepath.IsAbs(record.BinaryPath) || manager.cfg == nil {
		return record
	}
	if pluginDir := strings.TrimSpace(manager.cfg.Plugins.Dir); pluginDir != "" {
		record.BinaryPath = filepath.Join(pluginDir, filepath.Base(record.BinaryPath))
	}
	return record
}

// unavailableRuntime keeps a valid route visible when a plugin cannot be
// started at boot (for example, its executable is temporarily missing). The
// placeholder never participates in hooks; HTTP callers receive 502 while the
// manager retries the real child in the background.
func unavailableRuntime(record database.PluginRecord, cause error) *activePlugin {
	var manifest tdriveplugin.Manifest
	if err := json.Unmarshal([]byte(record.ManifestJSON), &manifest); err != nil {
		return nil
	}
	if err := manifest.Validate(); err != nil || manifest.APIVersion != tdriveplugin.APIVersion || manifest.ID != record.ID {
		return nil
	}
	if cause == nil {
		cause = errors.New("plugin runtime unavailable")
	}
	return &activePlugin{
		record:   record,
		manifest: manifest,
		client:   unavailablePluginClient{cause: cause},
		failed:   true,
	}
}

type unavailablePluginClient struct{ cause error }

func (client unavailablePluginClient) err() error {
	if client.cause == nil {
		return errors.New("plugin runtime unavailable")
	}
	return client.cause
}

func (client unavailablePluginClient) Before(context.Context, tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	return tdriveplugin.OperationResult{}, client.err()
}

func (client unavailablePluginClient) After(context.Context, tdriveplugin.Operation) error {
	return client.err()
}

func (client unavailablePluginClient) OnEvent(context.Context, tdriveplugin.Event) error {
	return client.err()
}

func (client unavailablePluginClient) HandleHTTP(context.Context, tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error) {
	return tdriveplugin.HTTPResponse{}, client.err()
}

func (unavailablePluginClient) Shutdown(context.Context) error { return nil }

// pluginDataDir gives every installed plugin a stable private directory. The
// server data directory is the persistence contract for the deployment; the
// plugin directory is only a fallback for package-level tests and older
// programmatic configurations that did not fill Server.DataDir.
func (manager *Manager) pluginDataDir(record database.PluginRecord) string {
	root := ""
	if manager.cfg != nil {
		if dataRoot := strings.TrimSpace(manager.cfg.Server.DataDir); dataRoot != "" {
			if absolute, err := filepath.Abs(dataRoot); err == nil {
				dataRoot = absolute
			}
			return filepath.Join(dataRoot, "plugin-data", record.ID)
		}
		root = strings.TrimSpace(manager.cfg.Plugins.Dir)
	}
	if root == "" {
		root = filepath.Dir(record.BinaryPath)
	}
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	// This is also the path used by the first host implementation, which had
	// no Server.DataDir in its in-memory test/development configuration. Keep
	// it stable so those installations do not need a data migration.
	return filepath.Join(root, record.ID+"-data")
}

// pluginEnvironment replaces a possibly inherited value rather than adding a
// duplicate. On Unix duplicate environment keys are legal but which one a
// child observes is implementation-dependent; that would make persistence
// depend on the host process environment.
func pluginEnvironment(dataDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, pluginDataDirEnv) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, pluginDataDirEnv+"="+dataDir)
}

func (manager *Manager) stopRuntime(ctx context.Context, active *activePlugin) {
	if active == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if active.client != nil {
		callCtx, cancel := pluginCallContext(ctx)
		if err := active.client.Shutdown(callCtx); err != nil {
			manager.log.Debug("plugin shutdown returned an error", zap.String("plugin", active.record.ID), zap.Error(err))
		}
		cancel()
	}
	if active.process != nil {
		active.process.Kill()
	}
}

func pluginCallContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, pluginCallTimeout)
}

func (manager *Manager) snapshotActive() []*activePlugin {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	plugins := make([]*activePlugin, 0, len(manager.active))
	for _, active := range manager.active {
		if active == nil || active.failed {
			continue
		}
		plugins = append(plugins, active)
	}
	sort.SliceStable(plugins, func(left, right int) bool {
		if plugins[left].record.InstalledAt.Equal(plugins[right].record.InstalledAt) {
			return plugins[left].record.ID < plugins[right].record.ID
		}
		return plugins[left].record.InstalledAt.Before(plugins[right].record.InstalledAt)
	})
	return plugins
}

func (manager *Manager) getActive(id string) *activePlugin {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.active[id]
}

func (manager *Manager) takeActive(id string) *activePlugin {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	active := manager.active[id]
	delete(manager.active, id)
	return active
}

func (manager *Manager) restoreActive(id string, active *activePlugin) {
	if active == nil {
		return
	}
	manager.mu.Lock()
	manager.active[id] = active
	manager.mu.Unlock()
	manager.refreshDriveHooks()
}

func (manager *Manager) refreshDriveHooks() {
	hasHooks := manager.HasHooks()
	manager.hooksEnabled.Store(hasHooks)
	if manager.drive == nil {
		return
	}
	if hasHooks {
		manager.drive.SetPluginHooks(manager)
		return
	}
	manager.drive.SetPluginHooks(nil)
}

func (manager *Manager) handlePluginFailure(id string, cause error) {
	if cause == nil {
		cause = errors.New("plugin runtime failed")
	}
	active := manager.getActive(id)
	if active == nil {
		// A concurrent disable/uninstall already removed the runtime. Do not
		// write an error using an old callback and accidentally re-enable that
		// plugin in the database.
		return
	}
	manager.scheduleRestart(active, cause)
}

// recordPluginFailure records the failure without changing whether the
// administrator enabled the plugin. A transient RPC error must not turn an
// enabled plugin into a permanently unreachable route.
func (manager *Manager) recordPluginFailure(id string, cause error) {
	if _, err := manager.db.UpdatePluginStatus(context.Background(), id, database.PluginStatusError, cause.Error()); err != nil {
		manager.log.Warn("could not record plugin failure", zap.String("plugin", id), zap.Error(err))
	}
}

// scheduleRestart replaces a failed child in the background. The failed
// activePlugin deliberately remains in manager.active until a replacement is
// ready, so a browser sees a temporary 502 rather than a misleading 404. The
// recovering guard also prevents concurrent HTTP polls, hooks and event
// deliveries from starting several copies of the same plugin.
func (manager *Manager) scheduleRestart(failed *activePlugin, cause error) {
	if failed == nil {
		return
	}
	if cause == nil {
		cause = errors.New("plugin runtime failed")
	}
	id := failed.record.ID
	manager.mu.Lock()
	if manager.closed || manager.active[id] != failed || manager.recovering[id] {
		manager.mu.Unlock()
		return
	}
	failed.failed = true
	manager.recovering[id] = true
	manager.recoveryWG.Add(1)
	manager.mu.Unlock()

	// Disable core hook interception immediately. The failed route remains in
	// manager.active for HTTP/502 and recovery, but unrelated core requests
	// should proceed while the child is being replaced.
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
	manager.recordPluginFailure(id, cause)
	go func() {
		defer manager.recoveryWG.Done()
		manager.restart(failed)
	}()
}

func (manager *Manager) restart(failed *activePlugin) {
	id := failed.record.ID
	baseCtx := manager.recoveryContext()
	stopCtx, stopCancel := context.WithTimeout(baseCtx, pluginCallTimeout)
	manager.stopRuntime(stopCtx, failed)
	stopCancel()

	startCtx, startCancel := context.WithTimeout(baseCtx, pluginCallTimeout)
	record, err := manager.db.PluginByID(startCtx, id)
	disabled := false
	if err == nil && !record.Enabled {
		disabled = true
		err = errors.New("plugin was disabled while it was recovering")
	}
	var replacement *activePlugin
	if err == nil {
		replacement, err = manager.startRuntime(startCtx, record)
	}
	startCancel()

	manager.mu.Lock()
	current := manager.active[id]
	canReplace := err == nil && current == failed && !manager.closed && record.Enabled
	if canReplace {
		manager.active[id] = replacement
	}
	if current == failed && err != nil && !disabled {
		// Keep the failed placeholder in the map. It still makes the declared
		// route reachable and a later request can trigger another recovery.
		manager.active[id] = failed
	}
	if current == failed && disabled {
		delete(manager.active, id)
	}
	if err != nil && current != failed {
		// Management code won the race and removed/replaced the child.
		canReplace = false
	}
	delete(manager.recovering, id)
	manager.mu.Unlock()

	if replacement != nil && !canReplace {
		manager.stopRuntime(context.Background(), replacement)
	}
	if err != nil {
		manager.log.Warn("could not recover plugin", zap.String("plugin", id), zap.Error(err))
		manager.refreshDriveHooks()
		return
	}
	if !canReplace {
		// A management operation, uninstall/disable, or shutdown won the race
		// while the replacement was starting. That operation owns the database
		// state; do not write "active" back over its result.
		manager.refreshDriveHooks()
		return
	}
	manager.refreshDriveHooks()
	manager.startEventBridge(context.Background())
	if _, stateErr := manager.db.UpdatePluginStatusIfEnabled(context.Background(), id, database.PluginStatusActive, ""); stateErr != nil && !errors.Is(stateErr, database.ErrNotFound) {
		manager.log.Warn("could not record recovered plugin", zap.String("plugin", id), zap.Error(stateErr))
	}
}

func (manager *Manager) recoveryContext() context.Context {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.recoveryCtx == nil {
		return context.Background()
	}
	return manager.recoveryCtx
}

func (manager *Manager) waitRecoveries(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		manager.recoveryWG.Wait()
		close(done)
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (manager *Manager) toStatus(record database.PluginRecord) PluginStatus {
	var manifest tdriveplugin.Manifest
	if err := json.Unmarshal([]byte(record.ManifestJSON), &manifest); err != nil {
		manifest = tdriveplugin.Manifest{
			ID:      record.ID,
			Name:    record.Name,
			Version: record.Version,
			Author:  record.Author,
		}
	}
	return PluginStatus{
		ID:             record.ID,
		Manifest:       manifest,
		Enabled:        record.Enabled,
		Status:         record.Status,
		Source:         record.Source,
		ManifestURL:    record.ManifestURL,
		ManifestDigest: record.ManifestDigest,
		BinaryDigest:   record.BinaryDigest,
		Error:          record.Error,
		InstalledAt:    record.InstalledAt,
		UpdatedAt:      record.UpdatedAt,
	}
}

// manifestsMatch compares what a running plugin says about itself with the
// manifest that was installed. Artifacts are excluded: they describe where the
// executable was downloaded from, which a plugin cannot report about itself,
// and the bytes are already pinned by the SHA-256 check at download time and
// again by go-plugin's SecureConfig before exec.
func manifestsMatch(left, right tdriveplugin.Manifest) bool {
	left.Artifacts = nil
	right.Artifacts = nil
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func (manager *Manager) startEventBridge(parent context.Context) {
	// Management calls arrive with short-lived HTTP contexts. The event
	// subscription must belong to the manager lifetime instead, or it would
	// stop as soon as an install/enable response is returned.
	_ = parent
	parent = manager.recoveryContext()
	manager.mu.Lock()
	if manager.eventsStop != nil || !manager.hasReadyPluginLocked() || manager.broker == nil || manager.closed {
		manager.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	manager.eventsStop = cancel
	manager.mu.Unlock()

	channel, release := manager.broker.Subscribe("")
	go func() {
		defer release()
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-channel:
				if !ok {
					return
				}
				manager.dispatchEvent(payload)
			}
		}
	}()
}

func (manager *Manager) stopEventBridgeIfEmpty() {
	manager.mu.Lock()
	if manager.hasReadyPluginLocked() {
		manager.mu.Unlock()
		return
	}
	stop := manager.eventsStop
	manager.eventsStop = nil
	manager.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (manager *Manager) dispatchEvent(payload []byte) {
	var event struct {
		Type   events.Type     `json:"type"`
		Data   json.RawMessage `json:"data"`
		At     int64           `json:"at"`
		UserID string          `json:"userId,omitempty"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	for _, active := range manager.snapshotActive() {
		if !declaresEvent(active.manifest, string(event.Type)) {
			continue
		}
		callCtx, cancel := context.WithTimeout(context.Background(), pluginCallTimeout)
		err := active.client.OnEvent(callCtx, tdriveplugin.Event{
			Type:   string(event.Type),
			Data:   event.Data,
			At:     time.UnixMilli(event.At),
			UserID: event.UserID,
		})
		cancel()
		if err != nil {
			manager.log.Warn("plugin event hook failed", zap.String("plugin", active.record.ID), zap.Error(err))
			manager.handlePluginFailure(active.record.ID, err)
		}
	}
}

func declaresEvent(manifest tdriveplugin.Manifest, eventType string) bool {
	for _, declared := range manifest.Events {
		if declared == "*" || declared == eventType {
			return true
		}
	}
	return false
}
