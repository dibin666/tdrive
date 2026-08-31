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
)

// Inspection is the one-time source review shown by the WebUI before an
// installation is confirmed. The source digest binds the later build to the
// exact tree inspected here.
type Inspection struct {
	ID             string                `json:"inspectionId"`
	Manifest       tdriveplugin.Manifest `json:"manifest"`
	SourceURL      string                `json:"sourceUrl"`
	Ref            string                `json:"ref,omitempty"`
	SourceDigest   string                `json:"sourceDigest"`
	Compatible     bool                  `json:"compatible"`
	IsUpdate       bool                  `json:"isUpdate"`
	CurrentVersion string                `json:"currentVersion,omitempty"`
	Warning        string                `json:"warning,omitempty"`
	ExpiresAt      time.Time             `json:"expiresAt"`
}

// StoreIndex is the intentionally boring JSON format consumed by the plugin
// store UI. A store only discovers source metadata; installation still goes
// through the same inspect-and-confirm flow as a manually entered URL.
type StoreIndex struct {
	UpdatedAt time.Time     `json:"updatedAt"`
	Plugins   []StorePlugin `json:"plugins"`
}

type StorePlugin struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Version       string   `json:"version"`
	Author        string   `json:"author"`
	RepositoryURL string   `json:"repositoryUrl"`
	SourceURL     string   `json:"sourceUrl,omitempty"`
	Ref           string   `json:"ref,omitempty"`
	SourceDigest  string   `json:"sourceDigest"`
	Documentation string   `json:"documentationUrl,omitempty"`
	License       string   `json:"license"`
	Tags          []string `json:"tags,omitempty"`
}

// PluginStatus is safe to return to the WebUI. The raw binary path and
// manifest JSON remain host-only fields.
type PluginStatus struct {
	ID           string                `json:"id"`
	Manifest     tdriveplugin.Manifest `json:"manifest"`
	Enabled      bool                  `json:"enabled"`
	Status       string                `json:"status"`
	Source       string                `json:"source"`
	SourceURL    string                `json:"sourceUrl,omitempty"`
	Ref          string                `json:"ref,omitempty"`
	SourceDigest string                `json:"sourceDigest"`
	BinaryDigest string                `json:"binaryDigest"`
	Error        string                `json:"error,omitempty"`
	InstalledAt  time.Time             `json:"installedAt"`
	UpdatedAt    time.Time             `json:"updatedAt"`
}

type activePlugin struct {
	record   database.PluginRecord
	manifest tdriveplugin.Manifest
	process  *goPlugin.Client
	protocol goPlugin.ClientProtocol
	client   *tdriveplugin.Client
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

	builder sourceBuilder

	installMu    sync.Mutex
	mu           sync.RWMutex
	hooksEnabled atomic.Bool
	active       map[string]*activePlugin
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

// New creates a manager without touching the plugin directory. The builder
// client is only a transport object; it does not connect or spawn anything
// until Inspect or Install is called.
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
	manager := &Manager{
		cfg:         cfg,
		db:          db,
		auth:        authSvc,
		drive:       driveSvc,
		tg:          tgm,
		broker:      broker,
		log:         log,
		active:      make(map[string]*activePlugin),
		inspections: make(map[string]pendingInspection),
	}
	if builder, err := newBuilderClient(cfg.Plugins); err == nil {
		manager.builder = builder
	} else {
		manager.log.Warn("plugin builder configuration is invalid", zap.Error(err))
	}
	return manager
}

// SetBuilder is intended for package tests and embedded deployments that
// provide their own builder implementation.
func (manager *Manager) SetBuilder(builder sourceBuilder) {
	manager.mu.Lock()
	manager.builder = builder
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

// Close gracefully stops plugin children and a builder started by this
// process. It is deliberately idempotent so shutdown paths can defer it.
func (manager *Manager) Close(ctx context.Context) error {
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
	builder := manager.builder
	manager.mu.Unlock()
	manager.refreshDriveHooks()

	if stopEvents != nil {
		stopEvents()
	}
	for _, plugin := range plugins {
		manager.stopRuntime(ctx, plugin)
	}
	if builder != nil {
		builder.Close()
	}
	return nil
}

// HasHooks is used by drive.Service to preserve a nil fast path when no
// active plugin implements the operation hook bridge.
func (manager *Manager) HasHooks() bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return len(manager.active) > 0
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
		callCtx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
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
		callCtx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
		err := active.client.After(callCtx, operation)
		cancel()
		if err != nil {
			manager.log.Warn("plugin after hook failed", zap.String("plugin", active.record.ID), zap.Error(err))
			manager.handlePluginFailure(active.record.ID, err)
		}
	}
}

// Inspect gets and validates a source tree and stores a one-time inspection
// token. The token is consumed by Install, so a confirmation cannot be replayed
// against a different source or silently rebuild a mutable branch.
func (manager *Manager) Inspect(ctx context.Context, sourceURL, ref string, expectedDigest ...string) (Inspection, error) {
	if _, err := ValidateSourceURL(sourceURL); err != nil {
		return Inspection{}, err
	}
	if err := ValidateRef(ref); err != nil {
		return Inspection{}, err
	}
	manager.mu.RLock()
	builder := manager.builder
	manager.mu.RUnlock()
	if builder == nil {
		return Inspection{}, errors.New("plugin builder is not configured")
	}
	request := BuilderRequest{SourceURL: sourceURL, Ref: ref}
	if len(expectedDigest) > 0 {
		request.ExpectedSourceDigest = expectedDigest[0]
	}
	result, err := builder.Inspect(ctx, request)
	if err != nil {
		return Inspection{}, err
	}
	compatible := result.Manifest.APIVersion == tdriveplugin.APIVersion
	if !compatible {
		return Inspection{}, fmt.Errorf("plugin API version %d is not supported; host uses %d", result.Manifest.APIVersion, tdriveplugin.APIVersion)
	}
	inspection := Inspection{
		ID:           database.NewID(),
		Manifest:     result.Manifest,
		SourceURL:    sourceURL,
		Ref:          ref,
		SourceDigest: result.SourceDigest,
		Compatible:   compatible,
		Warning:      "全信任插件可调用和修改 tdrive 暴露的全部功能，并可执行其构建出的程序。",
		ExpiresAt:    time.Now().Add(inspectionLifetime),
	}
	if current, err := manager.db.PluginByID(ctx, result.Manifest.ID); err == nil {
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
	stagingBinary := filepath.Join(stagingDir, "plugin")
	defer os.RemoveAll(stagingDir)

	manager.mu.RLock()
	builder := manager.builder
	manager.mu.RUnlock()
	if builder == nil {
		return PluginStatus{}, errors.New("plugin builder is not configured")
	}
	result, err := builder.Build(ctx, BuilderRequest{
		SourceURL:            inspection.SourceURL,
		Ref:                  inspection.Ref,
		ExpectedSourceDigest: inspection.SourceDigest,
		PluginID:             inspection.Manifest.ID,
		OutputPath:           stagingBinary,
		GOOS:                 runtime.GOOS,
		GOARCH:               runtime.GOARCH,
	})
	if err != nil {
		return PluginStatus{}, err
	}
	if result.SourceDigest != inspection.SourceDigest || !manifestsMatch(result.Manifest, inspection.Manifest) {
		return PluginStatus{}, errors.New("plugin source changed after inspection")
	}
	if result.BinaryDigest == "" {
		return PluginStatus{}, errors.New("plugin builder returned no binary digest")
	}

	manifestJSON, err := json.Marshal(result.Manifest)
	if err != nil {
		return PluginStatus{}, fmt.Errorf("encode plugin manifest: %w", err)
	}
	finalPath := filepath.Join(manager.cfg.Plugins.Dir, inspection.Manifest.ID)
	oldRecord, oldRecordErr := manager.db.PluginByID(ctx, inspection.Manifest.ID)
	if oldRecordErr != nil && !errors.Is(oldRecordErr, database.ErrNotFound) {
		return PluginStatus{}, fmt.Errorf("read existing plugin metadata: %w", oldRecordErr)
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
		ID:           result.Manifest.ID,
		Name:         result.Manifest.Name,
		Version:      result.Manifest.Version,
		Author:       result.Manifest.Author,
		Enabled:      true,
		Status:       database.PluginStatusActive,
		Source:       "source",
		SourceURL:    inspection.SourceURL,
		Ref:          inspection.Ref,
		SourceDigest: result.SourceDigest,
		BinaryDigest: result.BinaryDigest,
		BinaryPath:   finalPath,
		ManifestJSON: string(manifestJSON),
		InstalledAt:  now,
		UpdatedAt:    now,
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
		return manager.toStatus(updated), nil
	}
	loaded, err := manager.startRuntime(ctx, record)
	if err != nil {
		_, _ = manager.db.UpdatePluginState(ctx, id, true, database.PluginStatusError, err.Error())
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
	if _, err := os.Stat(record.BinaryPath); err != nil {
		return nil, fmt.Errorf("stat plugin binary: %w", err)
	}
	checksum, err := hex.DecodeString(record.BinaryDigest)
	if err != nil || len(checksum) != sha256.Size {
		return nil, errors.New("stored plugin binary digest is invalid")
	}

	process := goPlugin.NewClient(&goPlugin.ClientConfig{
		HandshakeConfig: tdriveplugin.HandshakeConfig,
		Plugins: goPlugin.PluginSet{
			tdriveplugin.PluginName: &tdriveplugin.RPCPlugin{},
		},
		Cmd:          exec.Command(record.BinaryPath),
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
	callCtx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
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
	callCtx, cancel = context.WithTimeout(ctx, pluginCallTimeout)
	err = client.AttachHost(callCtx, host)
	cancel()
	if err != nil {
		process.Kill()
		return nil, fmt.Errorf("initialize plugin: %w", err)
	}
	return &activePlugin{record: record, manifest: manifest, process: process, protocol: protocol, client: client}, nil
}

func (manager *Manager) stopRuntime(ctx context.Context, active *activePlugin) {
	if active == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	if err := active.client.Shutdown(callCtx); err != nil {
		manager.log.Debug("plugin shutdown returned an error", zap.String("plugin", active.record.ID), zap.Error(err))
	}
	cancel()
	active.process.Kill()
}

func (manager *Manager) snapshotActive() []*activePlugin {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	plugins := make([]*activePlugin, 0, len(manager.active))
	for _, active := range manager.active {
		plugins = append(plugins, active)
	}
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
	active := manager.takeActive(id)
	if active != nil {
		manager.stopRuntime(context.Background(), active)
	}
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
	if _, err := manager.db.UpdatePluginState(context.Background(), id, true, database.PluginStatusError, cause.Error()); err != nil {
		manager.log.Warn("could not record plugin failure", zap.String("plugin", id), zap.Error(err))
	}
	manager.startEventBridge(context.Background())
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
		ID:           record.ID,
		Manifest:     manifest,
		Enabled:      record.Enabled,
		Status:       record.Status,
		Source:       record.Source,
		SourceURL:    record.SourceURL,
		Ref:          record.Ref,
		SourceDigest: record.SourceDigest,
		BinaryDigest: record.BinaryDigest,
		Error:        record.Error,
		InstalledAt:  record.InstalledAt,
		UpdatedAt:    record.UpdatedAt,
	}
}

func manifestsMatch(left, right tdriveplugin.Manifest) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func (manager *Manager) startEventBridge(parent context.Context) {
	manager.mu.Lock()
	if manager.eventsStop != nil || len(manager.active) == 0 || manager.broker == nil || manager.closed {
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
	if len(manager.active) != 0 {
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
