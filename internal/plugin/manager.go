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

// ErrPluginLimit reports that a plugin was refused for lack of room rather
// than for anything wrong with the request. Every plugin is a child process
// holding an RPC connection, so per-account ownership turns the deployment's
// process budget from "one per plugin" into "one per plugin per account" —
// which needs a ceiling somewhere.
var ErrPluginLimit = errors.New("插件数量已达上限")

var errProcessLimitReached = fmt.Errorf("%w：已达到系统插件进程上限", ErrPluginLimit)

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

// pluginKey identifies one installed plugin. Plugins are owned per account, so
// the same id can be installed by several people at different versions, each
// with its own binary, child process and data directory. Every map, lookup and
// recovery path in this file is keyed by the pair rather than the id alone.
type pluginKey struct {
	userID   string
	pluginID string
}

func keyOf(record database.PluginRecord) pluginKey {
	return pluginKey{userID: record.UserID, pluginID: record.ID}
}

// String is only for log and error messages, where "whose aliyunpan" is the
// question a bare plugin id stopped being able to answer.
func (key pluginKey) String() string { return key.userID + "/" + key.pluginID }

type pendingInspection struct {
	inspection Inspection
	// userID is the account that asked for the review. Install checks it,
	// because the token is a record of one person having read and confirmed
	// a manifest; letting anybody else spend it would hand them somebody
	// else's confirmation.
	userID string
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
	active       map[pluginKey]*activePlugin
	recovering   map[pluginKey]bool
	recoveryCtx  context.Context
	recoveryStop context.CancelFunc
	recoveryWG   sync.WaitGroup
	inspections  map[string]pendingInspection
	eventsStop   context.CancelFunc
	closed       bool

	// updates caches the last release check. See updates.go.
	updates updateCache
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
		active:       make(map[pluginKey]*activePlugin),
		recovering:   make(map[pluginKey]bool),
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

// Start loads every account's enabled records and starts their
// already-installed binaries. A broken plugin is isolated and recorded as an
// error; it does not prevent the drive or WebUI from starting.
//
// The records arrive in a deterministic order, which matters once a process
// cap is in play: the same plugins win a contested boot every time rather than
// depending on map iteration.
func (manager *Manager) Start(ctx context.Context) error {
	records, err := manager.db.ListAllEnabledPlugins(ctx)
	if err != nil {
		return fmt.Errorf("list enabled plugins: %w", err)
	}
	started := 0
	for _, record := range records {
		key := keyOf(record)
		if limit := manager.processLimit(); limit > 0 && started >= limit {
			// Deliberately no scheduleRestart: retrying against a hard cap
			// would burn CPU forever. The row stays enabled, so raising the
			// cap or disabling something else brings this plugin back on the
			// next restart or the next explicit enable.
			manager.log.Warn("plugin not started because the process limit was reached",
				zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Int("limit", limit))
			if _, stateErr := manager.db.UpdatePluginState(ctx, record.UserID, record.ID,
				true, database.PluginStatusError, errProcessLimitReached.Error()); stateErr != nil {
				manager.log.Warn("could not record a plugin held back by the process limit",
					zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(stateErr))
			}
			continue
		}
		record = manager.relocateLegacyLayout(ctx, record)
		loaded, err := manager.startRuntime(ctx, record)
		if err != nil {
			manager.log.Error("plugin failed to start",
				zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(err))
			if _, stateErr := manager.db.UpdatePluginState(ctx, record.UserID, record.ID, true, database.PluginStatusError, err.Error()); stateErr != nil {
				manager.log.Warn("could not record plugin startup failure",
					zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(stateErr))
			}
			if placeholder := unavailableRuntime(record, err); placeholder != nil {
				manager.mu.Lock()
				if !manager.closed {
					manager.active[key] = placeholder
				}
				manager.mu.Unlock()
				manager.scheduleRestart(placeholder, err)
				// A placeholder holds the route open and its recovery will
				// spawn a real child, so it spends a slot. A record too broken
				// even for a placeholder spawns nothing and spends nothing.
				started++
			}
			continue
		}
		started++
		manager.mu.Lock()
		manager.active[key] = loaded
		manager.mu.Unlock()
		manager.refreshDriveHooks()
		if _, stateErr := manager.db.UpdatePluginState(ctx, record.UserID, record.ID, true, database.PluginStatusActive, ""); stateErr != nil {
			manager.log.Warn("could not record active plugin state",
				zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(stateErr))
		}
	}
	manager.startEventBridge(ctx)
	return nil
}

// relocateLegacyLayout moves a plugin installed under the old deployment-wide
// layout into its owner's directory, once.
//
// The schema migration attributes every carried-over row to one account but
// cannot touch the filesystem, so the binary is still at
// <plugins.dir>/<id> and the data at <data.dir>/plugin-data/<id>, both of
// which two accounts would otherwise contend for. Post-migration each row has
// exactly one owner, so the destination is unambiguous.
//
// Both moves are best effort. Losing a plugin's persisted state — the
// aliyunpan plugin keeps its OAuth tokens there — because a rename failed
// silently is far worse than an untidy directory tree, so a failure logs both
// paths and leaves the plugin running from where it already was. Nothing is
// deleted here; a rename is the only operation.
func (manager *Manager) relocateLegacyLayout(ctx context.Context, record database.PluginRecord) database.PluginRecord {
	if manager.cfg == nil || record.UserID == "" {
		return record
	}
	pluginDir := strings.TrimSpace(manager.cfg.Plugins.Dir)
	if pluginDir != "" && record.BinaryPath != "" {
		if absolute, err := filepath.Abs(pluginDir); err == nil {
			pluginDir = absolute
		}
		current := record.BinaryPath
		if absolute, err := filepath.Abs(current); err == nil {
			current = absolute
		}
		if filepath.Dir(current) == pluginDir {
			target := filepath.Join(pluginDir, record.UserID, filepath.Base(current))
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				manager.log.Warn("could not create the owner's plugin directory",
					zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(err))
			} else if err := os.Rename(current, target); err != nil {
				if !os.IsNotExist(err) {
					manager.log.Warn("could not move a plugin binary into its owner's directory",
						zap.String("user", record.UserID), zap.String("plugin", record.ID),
						zap.String("from", current), zap.String("to", target), zap.Error(err))
				}
			} else {
				record.BinaryPath = target
				if err := manager.db.UpsertPlugin(ctx, record); err != nil {
					manager.log.Warn("could not record the relocated plugin binary path",
						zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(err))
				}
			}
		}
	}

	target := manager.pluginDataDir(record)
	legacy := manager.legacyPluginDataDir(record)
	if legacy != "" && legacy != target && !directoryExists(target) && directoryExists(legacy) {
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			manager.log.Warn("could not create the owner's plugin data root",
				zap.String("user", record.UserID), zap.String("plugin", record.ID), zap.Error(err))
		} else if err := os.Rename(legacy, target); err != nil {
			manager.log.Warn("could not move a plugin data directory to its owner; the plugin keeps using the old one",
				zap.String("user", record.UserID), zap.String("plugin", record.ID),
				zap.String("from", legacy), zap.String("to", target), zap.Error(err))
		}
	}
	return record
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
	for key, plugin := range manager.active {
		plugins = append(plugins, plugin)
		delete(manager.active, key)
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

// Before chains the requesting account's active plugins in installation order.
// A plugin may reject the operation or replace its JSON payload for the next
// plugin and the core.
//
// Only the owner's plugins are consulted. Before per-account ownership a
// plugin was a deployment-wide component and seeing every request was the
// whole point; now an installation is one account's decision, and letting that
// account's code inspect and rewrite another account's API requests would make
// "install a plugin" mean "read everyone's traffic".
//
// The consequence, accepted deliberately: an operation with no authenticated
// user — background maintenance, an index rebuild, anything rooted at
// context.Background — now reaches no plugin at all. A "system operations are
// visible to everyone" escape hatch would hand back exactly the privilege this
// removes.
func (manager *Manager) Before(ctx context.Context, operation tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	if tdriveplugin.IsHostCall(ctx) {
		return tdriveplugin.OperationResult{Allowed: true, Payload: operation.Payload}, nil
	}
	owner := operationOwner(ctx, operation)
	if owner == "" {
		return tdriveplugin.OperationResult{Allowed: true, Payload: operation.Payload}, nil
	}
	plugins := manager.snapshotActiveFor(owner)
	result := tdriveplugin.OperationResult{Allowed: true, Payload: operation.Payload}
	for _, active := range plugins {
		operation.Payload = result.Payload
		callCtx, cancel := pluginCallContext(ctx)
		pluginResult, err := active.client.Before(callCtx, operation)
		cancel()
		if err != nil {
			// A caller that gave up on its own request has not told us anything
			// about the plugin. Restarting a healthy child for it would abandon
			// whatever work that child was doing for everybody else.
			if !callerAbandonedPluginCall(ctx, err) {
				manager.handlePluginFailure(keyOf(active.record), err)
			}
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
	owner := operationOwner(ctx, operation)
	if owner == "" {
		return
	}
	for _, active := range manager.snapshotActiveFor(owner) {
		callCtx, cancel := pluginCallContext(ctx)
		err := active.client.After(callCtx, operation)
		cancel()
		if err != nil {
			if callerAbandonedPluginCall(ctx, err) {
				continue
			}
			manager.log.Warn("plugin after hook failed", zap.String("plugin", active.record.ID), zap.Error(err))
			manager.handlePluginFailure(keyOf(active.record), err)
		}
	}
}

// operationOwner resolves whose plugins should see an operation. The field is
// filled by drive.beforePluginOperation and by HTTPMiddleware; the context is
// the fallback for callers that build an Operation by hand.
func operationOwner(ctx context.Context, operation tdriveplugin.Operation) string {
	if operation.UserID != "" {
		return operation.UserID
	}
	if ctx == nil {
		return ""
	}
	return tdriveplugin.UserIDFromContext(ctx)
}

// callerAbandonedPluginCall reports whether a failed plugin call ended because
// the caller's own context was cancelled rather than because the plugin did
// anything wrong.
//
// The RPC client races every call against the caller's context and returns
// ctx.Err() when that context wins, so a cancelled HTTP request, a client that
// disconnected, or a shutting-down request chain all surface here as an error
// from a plugin that may be perfectly healthy. Recovering from one of those
// kills a working child and everything it had in flight.
func callerAbandonedPluginCall(ctx context.Context, err error) bool {
	return ctx != nil && ctx.Err() != nil && errors.Is(err, context.Canceled)
}

// Inspect downloads and validates a plugin manifest and stores a one-time
// inspection token for the reviewing account. Nothing is executed here and no
// plugin binary is fetched: the review shown to that account is built purely
// from the manifest. The token is consumed by Install, so a confirmation
// cannot be replayed against a different manifest — nor spent by anybody else,
// which matters now that installing is a permission rather than a role.
func (manager *Manager) Inspect(ctx context.Context, userID, manifestURL string, expectedDigest ...string) (Inspection, error) {
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
		Warning:        "插件以 tdrive 宿主进程权限运行，拥有系统完全访问权限。",
		ExpiresAt:      time.Now().Add(inspectionLifetime),
	}
	if current, err := manager.db.PluginByID(ctx, userID, manifest.ID); err == nil {
		inspection.IsUpdate = true
		inspection.CurrentVersion = current.Version
	}
	manager.mu.Lock()
	manager.inspections[inspection.ID] = pendingInspection{inspection: inspection, userID: userID}
	manager.mu.Unlock()
	return inspection, nil
}

// Install consumes an inspection token and activates the resulting plugin.
// The caller must have already performed the one UI confirmation; this method
// intentionally accepts no second permission or capability choice.
func (manager *Manager) Install(ctx context.Context, userID, inspectionID string) (PluginStatus, error) {
	manager.installMu.Lock()
	defer manager.installMu.Unlock()

	inspection, err := manager.consumeInspection(inspectionID, userID)
	if err != nil {
		return PluginStatus{}, err
	}
	if !inspection.Compatible || time.Now().After(inspection.ExpiresAt) {
		return PluginStatus{}, errors.New("plugin inspection is no longer valid")
	}
	key := pluginKey{userID: userID, pluginID: inspection.Manifest.ID}

	// Check the caps before a single byte is downloaded: refusing after the
	// download would spend the bandwidth anyway. An update replaces a process
	// rather than adding one, so it is exempt from both.
	existing, existingErr := manager.db.PluginByID(ctx, userID, inspection.Manifest.ID)
	if existingErr != nil && !errors.Is(existingErr, database.ErrNotFound) {
		return PluginStatus{}, fmt.Errorf("read existing plugin metadata: %w", existingErr)
	}
	if errors.Is(existingErr, database.ErrNotFound) {
		if err := manager.checkInstallLimits(ctx, userID); err != nil {
			return PluginStatus{}, err
		}
	}

	binDir := manager.userPluginDir(userID)
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		return PluginStatus{}, fmt.Errorf("create plugin directory: %w", err)
	}
	// Staging stays under one top-level directory so a single sweep cleans it,
	// but the name carries the owner: two accounts installing the same plugin
	// at the same moment must not share a staging path.
	stagingDir := filepath.Join(manager.cfg.Plugins.Dir, ".staging", userID+"-"+inspection.Manifest.ID+"-"+database.NewID())
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
	finalPath := filepath.Join(binDir, executableName(inspection.Manifest.ID, runtime.GOOS))
	oldRecord, oldRecordErr := existing, existingErr
	if oldRecordErr == nil {
		oldRecord = manager.normalizePluginRecord(oldRecord)
	}
	// #region DEBUG H4 plugin update filesystem paths
	writePluginDebugLog("H4", "internal/plugin/manager.go:440", "plugin update paths resolved", map[string]any{
		"pluginID":      inspection.Manifest.ID,
		"userID":        userID,
		"isUpdate":      oldRecordErr == nil,
		"finalPath":     finalPath,
		"finalPresent":  fileExists(finalPath),
		"oldBinaryPath": oldRecord.BinaryPath,
		"oldPresent":    oldRecordErr == nil && fileExists(oldRecord.BinaryPath),
		"stagingBinary": stagingBinary,
	})
	// #endregion
	oldActive := manager.takeActive(key)
	if oldActive != nil {
		manager.stopRuntime(ctx, oldActive)
	}
	manager.refreshDriveHooks()

	backupPath := finalPath + ".old-" + database.NewID()
	hadOldBinary := false
	if _, statErr := os.Stat(finalPath); statErr == nil {
		if err := os.Rename(finalPath, backupPath); err != nil {
			if oldActive != nil {
				manager.restoreRuntime(ctx, key, oldRecord)
			}
			return PluginStatus{}, fmt.Errorf("stage existing plugin binary: %w", err)
		}
		hadOldBinary = true
	} else if !os.IsNotExist(statErr) {
		if oldActive != nil {
			manager.restoreRuntime(ctx, key, oldRecord)
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
					manager.log.Warn("could not restore previous plugin metadata", zap.String("plugin", key.String()), zap.Error(restoreErr))
				}
			} else if deleteErr := manager.db.DeletePlugin(ctx, userID, inspection.Manifest.ID); deleteErr != nil && !errors.Is(deleteErr, database.ErrNotFound) {
				manager.log.Warn("could not remove failed plugin metadata", zap.String("plugin", key.String()), zap.Error(deleteErr))
			}
		}
		if oldActive != nil {
			manager.restoreRuntime(ctx, key, oldRecord)
		}
	}
	if err := os.Rename(stagingBinary, finalPath); err != nil {
		restoreOld()
		return PluginStatus{}, fmt.Errorf("install plugin binary: %w", err)
	}

	now := time.Now()
	record := database.PluginRecord{
		UserID:         userID,
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
	if _, err := manager.db.UpdatePluginState(ctx, userID, record.ID, true, database.PluginStatusActive, ""); err != nil {
		manager.stopRuntime(ctx, loaded)
		restoreOld()
		return PluginStatus{}, err
	}
	record.Status = database.PluginStatusActive
	manager.mu.Lock()
	manager.active[key] = loaded
	manager.mu.Unlock()
	// #region DEBUG H4 plugin update runtime installed
	writePluginDebugLog("H4", "internal/plugin/manager.go:506", "plugin runtime replaced after install", map[string]any{
		"pluginID":       record.ID,
		"finalPath":      finalPath,
		"pluginDataDir":  manager.pluginDataDir(record),
		"binaryPresent":  fileExists(finalPath),
		"oldActiveFound": oldActive != nil,
	})
	// #endregion
	manager.refreshDriveHooks()
	if hadOldBinary {
		_ = os.Remove(backupPath)
	}
	// An upgrade whose predecessor was installed under a different file name
	// leaves that file behind, because the backup dance above only knows about
	// finalPath. This happens when the naming rule itself changed — a Windows
	// plugin installed before the .exe suffix was added — and after the move to
	// per-account directories, where the predecessor sat in the flat
	// deployment-wide layout.
	//
	// That second case is why the path is checked against every other row
	// first. Two accounts carried over from the flat layout can both point at
	// <plugins.dir>/<id>, and removing it here would delete the binary the
	// other account is still running.
	if oldRecordErr == nil && oldRecord.BinaryPath != "" && oldRecord.BinaryPath != finalPath {
		switch shared, err := manager.binaryPathInUse(ctx, oldRecord.BinaryPath, key); {
		case err != nil:
			manager.log.Warn("could not check whether a plugin binary is shared; leaving it in place",
				zap.String("plugin", key.String()), zap.String("path", oldRecord.BinaryPath), zap.Error(err))
		case shared:
			manager.log.Info("left a plugin binary in place because another account's installation still points at it",
				zap.String("plugin", key.String()), zap.String("path", oldRecord.BinaryPath))
		default:
			if err := os.Remove(oldRecord.BinaryPath); err != nil && !os.IsNotExist(err) {
				manager.log.Warn("could not remove the previously installed plugin binary",
					zap.String("plugin", key.String()), zap.String("path", oldRecord.BinaryPath), zap.Error(err))
			}
		}
	}
	manager.startEventBridge(ctx)
	// The cached report still describes the version that was installed a moment
	// ago, and leaving it in place would keep offering an update that has just
	// been applied.
	manager.invalidateUpdatesFor(userID)
	return manager.toStatus(record), nil
}

// consumeInspection spends a review token, once, for the account that created
// it. The owner check is what keeps a confirmation personal: installing is a
// permission rather than a role now, so without it any holder of that
// permission could complete an installation somebody else reviewed and
// approved. A token offered by the wrong account is reported as missing so it
// cannot be used to probe what other people are considering installing.
func (manager *Manager) consumeInspection(id, userID string) (Inspection, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	pending, ok := manager.inspections[id]
	if !ok || pending.userID != userID {
		return Inspection{}, errors.New("plugin inspection was not found or already used")
	}
	delete(manager.inspections, id)
	return pending.inspection, nil
}

// List returns one account's installed records, including disabled and failed
// plugins.
func (manager *Manager) List(ctx context.Context, userID string) ([]PluginStatus, error) {
	records, err := manager.db.ListPlugins(ctx, userID)
	if err != nil {
		return nil, err
	}
	statuses := make([]PluginStatus, 0, len(records))
	for _, record := range records {
		statuses = append(statuses, manager.toStatus(record))
	}
	return statuses, nil
}

// SetEnabled starts or stops one of an account's installed plugins.
func (manager *Manager) SetEnabled(ctx context.Context, userID, id string, enabled bool) (PluginStatus, error) {
	record, err := manager.db.PluginByID(ctx, userID, id)
	if err != nil {
		return PluginStatus{}, err
	}
	key := keyOf(record)
	if !enabled {
		active := manager.takeActive(key)
		if active != nil {
			manager.stopRuntime(ctx, active)
		}
		manager.refreshDriveHooks()
		manager.stopEventBridgeIfEmpty()
		updated, err := manager.db.UpdatePluginState(ctx, userID, id, false, database.PluginStatusDisabled, "")
		if err != nil {
			return PluginStatus{}, err
		}
		manager.startEventBridge(ctx)
		return manager.toStatus(updated), nil
	}

	if active := manager.getActive(key); active != nil {
		updated, err := manager.db.UpdatePluginState(ctx, userID, id, true, database.PluginStatusActive, "")
		if err != nil {
			return PluginStatus{}, err
		}
		if active.failed {
			manager.scheduleRestart(active, errors.New("插件所有者请求重启"))
			if current, readErr := manager.db.PluginByID(context.Background(), userID, id); readErr == nil {
				return manager.toStatus(current), nil
			}
		}
		return manager.toStatus(updated), nil
	}
	// Starting is what spends a process slot, so this is where the
	// deployment-wide cap applies rather than at install time.
	if err := manager.checkProcessLimit(); err != nil {
		return PluginStatus{}, err
	}
	loaded, err := manager.startRuntime(ctx, record)
	if err != nil {
		_, _ = manager.db.UpdatePluginState(ctx, userID, id, true, database.PluginStatusError, err.Error())
		if placeholder := unavailableRuntime(record, err); placeholder != nil {
			manager.mu.Lock()
			if !manager.closed {
				manager.active[key] = placeholder
			}
			manager.mu.Unlock()
			manager.scheduleRestart(placeholder, err)
		}
		return PluginStatus{}, err
	}
	manager.mu.Lock()
	manager.active[key] = loaded
	manager.mu.Unlock()
	manager.refreshDriveHooks()
	updated, err := manager.db.UpdatePluginState(ctx, userID, id, true, database.PluginStatusActive, "")
	if err != nil {
		manager.handlePluginFailure(key, err)
		return PluginStatus{}, err
	}
	manager.startEventBridge(ctx)
	return manager.toStatus(updated), nil
}

// Uninstall stops a plugin, removes its binary and deletes its metadata. The
// database row is deleted last so a failed filesystem operation leaves a
// recoverable record rather than a half-uninstalled state.
func (manager *Manager) Uninstall(ctx context.Context, userID, id string) error {
	record, err := manager.db.PluginByID(ctx, userID, id)
	if err != nil {
		return err
	}
	record = manager.normalizePluginRecord(record)
	key := keyOf(record)
	active := manager.takeActive(key)
	if active != nil {
		manager.stopRuntime(ctx, active)
	}
	manager.refreshDriveHooks()
	backupPath := ""
	if record.BinaryPath != "" {
		backupPath = record.BinaryPath + ".uninstall-" + database.NewID()
		if err := os.Rename(record.BinaryPath, backupPath); err != nil && !os.IsNotExist(err) {
			if active != nil {
				manager.restoreRuntime(ctx, key, record)
			}
			return fmt.Errorf("stage plugin binary for removal: %w", err)
		}
	}
	if err := manager.db.DeletePlugin(ctx, userID, id); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, record.BinaryPath)
		}
		if active != nil {
			manager.restoreRuntime(ctx, key, record)
		}
		return err
	}
	if backupPath != "" {
		_ = os.Remove(backupPath)
	}
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
	manager.startEventBridge(ctx)
	manager.invalidateUpdatesFor(userID)
	return nil
}

func (manager *Manager) restoreRuntime(ctx context.Context, key pluginKey, record database.PluginRecord) {
	restarted, err := manager.startRuntime(ctx, record)
	if err != nil {
		manager.log.Warn("could not restore plugin after a failed management action", zap.String("plugin", key.String()), zap.Error(err))
		return
	}
	manager.mu.Lock()
	manager.active[key] = restarted
	manager.mu.Unlock()
	manager.refreshDriveHooks()
}

// Settings reads opaque JSON state for one account's plugin.
func (manager *Manager) Settings(ctx context.Context, userID, id string) (json.RawMessage, error) {
	if _, err := manager.db.PluginByID(ctx, userID, id); err != nil {
		return nil, err
	}
	value, err := manager.db.PluginData(ctx, userID, id, "settings")
	if errors.Is(err, database.ErrNotFound) {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(value), err
}

// UpdateSettings stores opaque JSON state for one account's plugin.
func (manager *Manager) UpdateSettings(ctx context.Context, userID, id string, value json.RawMessage) error {
	if _, err := manager.db.PluginByID(ctx, userID, id); err != nil {
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
	return manager.db.SetPluginData(ctx, userID, id, "settings", canonical)
}

// RemoveAllForUser tears down everything one account installed.
//
// It runs before the account row is deleted. The ON DELETE CASCADE on
// plugins.user_id removes the metadata, but a database delete cannot stop a
// child process still running with tdrive's privileges, and it cannot reclaim
// a binary or a data directory. A metadata failure is returned so the caller
// abandons the deletion — an account with its plugins intact is recoverable;
// orphaned processes with no owner are not.
func (manager *Manager) RemoveAllForUser(ctx context.Context, userID string) error {
	records, err := manager.db.ListPlugins(ctx, userID)
	if err != nil {
		return err
	}
	for _, record := range records {
		record = manager.normalizePluginRecord(record)
		if active := manager.takeActive(keyOf(record)); active != nil {
			manager.stopRuntime(ctx, active)
		}
		// The binary is renamed aside rather than deleted outright, so a failed
		// row delete can put it back. Deleting first would leave the row
		// pointing at a file that no longer exists, which is the one state the
		// caller cannot recover from by retrying.
		backupPath := ""
		if record.BinaryPath != "" {
			backupPath = record.BinaryPath + ".uninstall-" + database.NewID()
			if err := os.Rename(record.BinaryPath, backupPath); err != nil {
				if !os.IsNotExist(err) {
					manager.log.Warn("could not stage a deleted account's plugin binary for removal",
						zap.String("user", userID), zap.String("plugin", record.ID),
						zap.String("path", record.BinaryPath), zap.Error(err))
				}
				backupPath = ""
			}
		}
		if err := manager.db.DeletePlugin(ctx, userID, record.ID); err != nil && !errors.Is(err, database.ErrNotFound) {
			if backupPath != "" {
				_ = os.Rename(backupPath, record.BinaryPath)
			}
			manager.refreshDriveHooks()
			return fmt.Errorf("remove plugin %q: %w", record.ID, err)
		}
		if backupPath != "" {
			_ = os.Remove(backupPath)
		}
	}
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
	// Directory removal is best effort: the processes are gone and the rows
	// are gone, which is what "the account no longer has plugins" means. A
	// leftover directory is untidy, not unsafe.
	for _, dir := range manager.userDirectories(userID) {
		if err := os.RemoveAll(dir); err != nil {
			manager.log.Warn("could not remove a deleted account's plugin directory",
				zap.String("user", userID), zap.String("path", dir), zap.Error(err))
		}
	}
	manager.invalidateUpdatesFor(userID)
	return nil
}

// StopAllForUser stops an account's plugin children without touching their
// rows or their files. A disabled account cannot log in, but its plugins would
// otherwise keep running with the host's privileges — an account whose code
// still executes is not disabled. Re-enabling the account brings them back at
// the next restart or the next explicit enable.
func (manager *Manager) StopAllForUser(ctx context.Context, userID string) {
	manager.mu.Lock()
	stopping := make([]*activePlugin, 0)
	for key, active := range manager.active {
		if key.userID != userID {
			continue
		}
		stopping = append(stopping, active)
		delete(manager.active, key)
	}
	manager.mu.Unlock()
	for _, active := range stopping {
		manager.stopRuntime(ctx, active)
	}
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
}

func (manager *Manager) startRuntime(ctx context.Context, record database.PluginRecord) (*activePlugin, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	originalBinaryPath := record.BinaryPath
	record = manager.normalizePluginRecord(record)
	// #region DEBUG H1/H4 plugin record path normalization
	writePluginDebugLog("H1", "internal/plugin/manager.go:710", "plugin record path normalized", map[string]any{
		"pluginID":             record.ID,
		"originalBinaryPath":   originalBinaryPath,
		"normalizedBinaryPath": record.BinaryPath,
		"pathChanged":          originalBinaryPath != record.BinaryPath,
		"pluginDir":             manager.cfg.Plugins.Dir,
		"serverDataDir":         manager.cfg.Server.DataDir,
	})
	// #endregion
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
	binaryInfo, binaryStatErr := os.Stat(record.BinaryPath)
	// #region DEBUG H4 plugin executable filesystem check
	writePluginDebugLog("H4", "internal/plugin/manager.go:737", "plugin executable filesystem check", map[string]any{
		"pluginID":   record.ID,
		"binaryPath": record.BinaryPath,
		"statOK":     binaryStatErr == nil,
		"notFound":   os.IsNotExist(binaryStatErr),
		"isDirectory": binaryStatErr == nil && binaryInfo.IsDir(),
		"size":       pluginBinarySize(binaryInfo),
	})
	// #endregion
	if binaryStatErr != nil {
		return nil, fmt.Errorf("stat plugin binary: %w", binaryStatErr)
	}
	if binaryInfo.IsDir() {
		return nil, fmt.Errorf("plugin binary path is a directory: %s", record.BinaryPath)
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
	// #region DEBUG H1/H4 plugin child data directory
	writePluginDebugLog("H1", "internal/plugin/manager.go:764", "plugin child data directory resolved", map[string]any{
		"pluginID":       record.ID,
		"binaryPath":     record.BinaryPath,
		"pluginDataDir":  dataDir,
		"dataDirExists":  directoryExists(dataDir),
		"dataDirEnvKey":  pluginDataDirEnv,
	})
	// #endregion

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
		// #region DEBUG H4 plugin child connection failed
		writePluginDebugLog("H4", "internal/plugin/manager.go:794", "plugin child connection failed", map[string]any{
			"pluginID":   record.ID,
			"binaryPath": record.BinaryPath,
			"dataDir":    dataDir,
		})
		// #endregion
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
	host := &managerHost{manager: manager, pluginID: record.ID, userID: record.UserID}
	callCtx, cancel = pluginCallContext(ctx)
	err = client.AttachHost(callCtx, host)
	cancel()
	if err != nil {
		process.Kill()
		return nil, fmt.Errorf("initialize plugin: %w", err)
	}
	// #region DEBUG H4 plugin child initialized
	writePluginDebugLog("H4", "internal/plugin/manager.go:829", "plugin child initialized", map[string]any{
		"pluginID":      record.ID,
		"binaryPath":    record.BinaryPath,
		"pluginDataDir": dataDir,
	})
	// #endregion
	return &activePlugin{record: record, manifest: manifest, process: process, protocol: protocol, client: client}, nil
}

func pluginBinarySize(info os.FileInfo) int64 {
	if info == nil {
		return -1
	}
	return info.Size()
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (manager *Manager) normalizePluginRecord(record database.PluginRecord) database.PluginRecord {
	if record.BinaryPath == "" || filepath.IsAbs(record.BinaryPath) || manager.cfg == nil {
		return record
	}
	if pluginDir := strings.TrimSpace(manager.cfg.Plugins.Dir); pluginDir != "" {
		record.BinaryPath = filepath.Join(pluginDir, record.UserID, filepath.Base(record.BinaryPath))
	}
	return record
}

// userPluginDir is where one account's plugin executables live. Namespacing by
// account is what lets two people install the same plugin id at different
// versions without one overwriting the other's binary.
func (manager *Manager) userPluginDir(userID string) string {
	if manager.cfg == nil {
		return userID
	}
	return filepath.Join(manager.cfg.Plugins.Dir, userID)
}

// userDirectories lists the per-account trees removed when an account is
// deleted.
func (manager *Manager) userDirectories(userID string) []string {
	if manager.cfg == nil || userID == "" {
		return nil
	}
	dirs := make([]string, 0, 2)
	if pluginDir := strings.TrimSpace(manager.cfg.Plugins.Dir); pluginDir != "" {
		dirs = append(dirs, filepath.Join(pluginDir, userID))
	}
	if dataRoot := strings.TrimSpace(manager.cfg.Server.DataDir); dataRoot != "" {
		if absolute, err := filepath.Abs(dataRoot); err == nil {
			dataRoot = absolute
		}
		dirs = append(dirs, filepath.Join(dataRoot, "plugin-data", userID))
	}
	return dirs
}

// binaryPathInUse reports whether an installation other than `except` still
// points at a binary. Only rows carried over from the flat, deployment-wide
// layout can collide this way; anything installed since is already namespaced
// by account.
func (manager *Manager) binaryPathInUse(ctx context.Context, path string, except pluginKey) (bool, error) {
	if path == "" {
		return false, nil
	}
	records, err := manager.db.ListAllEnabledPlugins(ctx)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if keyOf(record) == except {
			continue
		}
		if manager.normalizePluginRecord(record).BinaryPath == path {
			return true, nil
		}
	}
	return false, nil
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

// pluginDataDir gives every installed plugin a stable private directory,
// namespaced by its owner so two accounts running the same plugin keep
// separate state. The server data directory is the persistence contract for
// the deployment; the plugin directory is only a fallback for package-level
// tests and older programmatic configurations that did not fill
// Server.DataDir.
func (manager *Manager) pluginDataDir(record database.PluginRecord) string {
	root := ""
	if manager.cfg != nil {
		if dataRoot := strings.TrimSpace(manager.cfg.Server.DataDir); dataRoot != "" {
			if absolute, err := filepath.Abs(dataRoot); err == nil {
				dataRoot = absolute
			}
			return filepath.Join(dataRoot, "plugin-data", record.UserID, record.ID)
		}
		root = strings.TrimSpace(manager.cfg.Plugins.Dir)
	}
	if root == "" {
		root = filepath.Dir(record.BinaryPath)
	}
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	// This mirrors the path used by the first host implementation, which had
	// no Server.DataDir in its in-memory test/development configuration.
	return filepath.Join(root, record.UserID, record.ID+"-data")
}

// legacyPluginDataDir is where a plugin's state lived before ownership was
// per-account. It exists only so Start can move that directory once; nothing
// else should resolve a path through it.
func (manager *Manager) legacyPluginDataDir(record database.PluginRecord) string {
	if manager.cfg == nil {
		return ""
	}
	if dataRoot := strings.TrimSpace(manager.cfg.Server.DataDir); dataRoot != "" {
		if absolute, err := filepath.Abs(dataRoot); err == nil {
			dataRoot = absolute
		}
		return filepath.Join(dataRoot, "plugin-data", record.ID)
	}
	root := strings.TrimSpace(manager.cfg.Plugins.Dir)
	if root == "" {
		return ""
	}
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	return filepath.Join(root, record.ID+"-data")
}

// pluginEnvironment replaces a possibly inherited value rather than adding a
// duplicate. On Unix duplicate environment keys are legal but which one a
// child observes is implementation-dependent; that would make persistence
// depend on the host process environment.
func pluginEnvironment(dataDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	removedOverrideCount := 0
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, pluginDataDirEnv) {
			removedOverrideCount++
			continue
		}
		env = append(env, entry)
	}
	// #region DEBUG H1 child environment construction
	writePluginDebugLog("H1", "internal/plugin/manager.go:882", "plugin child environment constructed", map[string]any{
		"dataDir":              dataDir,
		"removedInheritedKeys": removedOverrideCount,
		"appendedOverride":     true,
	})
	// #endregion
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

// snapshotActiveFor is the dispatch set for one account's traffic: the plugins
// that account installed, in installation order.
func (manager *Manager) snapshotActiveFor(userID string) []*activePlugin {
	return manager.snapshot(func(key pluginKey) bool { return key.userID == userID })
}

// snapshotAllActive spans every account. It is only correct for shutdown and
// for events that are facts about the deployment rather than about somebody's
// files.
func (manager *Manager) snapshotAllActive() []*activePlugin {
	return manager.snapshot(func(pluginKey) bool { return true })
}

func (manager *Manager) snapshot(include func(pluginKey) bool) []*activePlugin {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	plugins := make([]*activePlugin, 0, len(manager.active))
	for key, active := range manager.active {
		if active == nil || active.failed || !include(key) {
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

func (manager *Manager) getActive(key pluginKey) *activePlugin {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.active[key]
}

func (manager *Manager) takeActive(key pluginKey) *activePlugin {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	active := manager.active[key]
	delete(manager.active, key)
	return active
}

func (manager *Manager) restoreActive(key pluginKey, active *activePlugin) {
	if active == nil {
		return
	}
	manager.mu.Lock()
	manager.active[key] = active
	manager.mu.Unlock()
	manager.refreshDriveHooks()
}

// processLimit is the deployment-wide ceiling on plugin children. Per-account
// ownership turns the process budget from M plugins into N accounts x M
// plugins, which is a different order of magnitude.
func (manager *Manager) processLimit() int {
	if manager.cfg == nil {
		return 0
	}
	return manager.cfg.Plugins.MaxProcesses
}

func (manager *Manager) checkProcessLimit() error {
	limit := manager.processLimit()
	if limit <= 0 {
		return nil
	}
	manager.mu.RLock()
	running := len(manager.active)
	manager.mu.RUnlock()
	if running >= limit {
		return errProcessLimitReached
	}
	return nil
}

// checkInstallLimits applies both caps to a new installation: the account's own
// allowance and the deployment's process budget.
func (manager *Manager) checkInstallLimits(ctx context.Context, userID string) error {
	if err := manager.checkProcessLimit(); err != nil {
		return err
	}
	if manager.cfg == nil || manager.cfg.Plugins.MaxPerUser <= 0 {
		return nil
	}
	installed, err := manager.db.CountPluginsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if installed >= manager.cfg.Plugins.MaxPerUser {
		return fmt.Errorf("%w：单个账号最多安装 %d 个插件", ErrPluginLimit, manager.cfg.Plugins.MaxPerUser)
	}
	return nil
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

func (manager *Manager) handlePluginFailure(key pluginKey, cause error) {
	if cause == nil {
		cause = errors.New("plugin runtime failed")
	}
	active := manager.getActive(key)
	if active == nil {
		// A concurrent disable/uninstall already removed the runtime. Do not
		// write an error using an old callback and accidentally re-enable that
		// plugin in the database.
		return
	}
	manager.scheduleRestart(active, cause)
}

// recordPluginFailure records the failure without changing whether the owner
// enabled the plugin. A transient RPC error must not turn an enabled plugin
// into a permanently unreachable route.
func (manager *Manager) recordPluginFailure(key pluginKey, cause error) {
	if _, err := manager.db.UpdatePluginStatus(context.Background(), key.userID, key.pluginID, database.PluginStatusError, cause.Error()); err != nil {
		manager.log.Warn("could not record plugin failure", zap.String("plugin", key.String()), zap.Error(err))
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
	key := keyOf(failed.record)
	manager.mu.Lock()
	if manager.closed || manager.active[key] != failed || manager.recovering[key] {
		manager.mu.Unlock()
		return
	}
	failed.failed = true
	manager.recovering[key] = true
	manager.recoveryWG.Add(1)
	manager.mu.Unlock()

	// Disable core hook interception immediately. The failed route remains in
	// manager.active for HTTP/502 and recovery, but unrelated core requests
	// should proceed while the child is being replaced.
	manager.refreshDriveHooks()
	manager.stopEventBridgeIfEmpty()
	manager.recordPluginFailure(key, cause)
	go func() {
		defer manager.recoveryWG.Done()
		manager.restart(failed)
	}()
}

func (manager *Manager) restart(failed *activePlugin) {
	key := keyOf(failed.record)
	baseCtx := manager.recoveryContext()
	stopCtx, stopCancel := context.WithTimeout(baseCtx, pluginCallTimeout)
	manager.stopRuntime(stopCtx, failed)
	stopCancel()

	startCtx, startCancel := context.WithTimeout(baseCtx, pluginCallTimeout)
	record, err := manager.db.PluginByID(startCtx, key.userID, key.pluginID)
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
	current := manager.active[key]
	canReplace := err == nil && current == failed && !manager.closed && record.Enabled
	if canReplace {
		manager.active[key] = replacement
	}
	if current == failed && err != nil && !disabled {
		// Keep the failed placeholder in the map. It still makes the declared
		// route reachable and a later request can trigger another recovery.
		manager.active[key] = failed
	}
	if current == failed && disabled {
		delete(manager.active, key)
	}
	if err != nil && current != failed {
		// Management code won the race and removed/replaced the child.
		canReplace = false
	}
	delete(manager.recovering, key)
	manager.mu.Unlock()

	if replacement != nil && !canReplace {
		manager.stopRuntime(context.Background(), replacement)
	}
	if err != nil {
		manager.log.Warn("could not recover plugin", zap.String("plugin", key.String()), zap.Error(err))
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
	if _, stateErr := manager.db.UpdatePluginStatusIfEnabled(context.Background(), key.userID, key.pluginID, database.PluginStatusActive, ""); stateErr != nil && !errors.Is(stateErr, database.ErrNotFound) {
		manager.log.Warn("could not record recovered plugin", zap.String("plugin", key.String()), zap.Error(stateErr))
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
	for _, active := range manager.eventTargets(event.Type, event.UserID) {
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
			manager.handlePluginFailure(keyOf(active.record), err)
		}
	}
}

// eventTargets decides which plugins hear an event.
//
// An event addressed to an account goes to that account's plugins. An event
// with no account is broadcast, but only when it is a fact about the
// deployment rather than about somebody's files: a Telegram connection change
// or an index rebuild says nothing about any one person, whereas a tree event
// carries a path and is published without a user id in most places. Handing
// that to every account's plugins would leak one person's directory names to
// another person's code, which is exactly what per-account ownership is for.
func (manager *Manager) eventTargets(eventType events.Type, userID string) []*activePlugin {
	if userID != "" {
		return manager.snapshotActiveFor(userID)
	}
	switch eventType {
	case events.TypeTelegram, events.TypeIndex:
		return manager.snapshotAllActive()
	default:
		return nil
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
