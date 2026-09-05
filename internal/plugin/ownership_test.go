package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// recordingClient answers every call and remembers that it was asked, which is
// what the isolation tests assert on.
type recordingClient struct{ before, after, events int }

func (client *recordingClient) Before(context.Context, tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	client.before++
	return tdriveplugin.OperationResult{Allowed: true}, nil
}

func (client *recordingClient) After(context.Context, tdriveplugin.Operation) error {
	client.after++
	return nil
}

func (client *recordingClient) OnEvent(context.Context, tdriveplugin.Event) error {
	client.events++
	return nil
}

func (client *recordingClient) HandleHTTP(context.Context, tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error) {
	return tdriveplugin.HTTPResponse{Status: http.StatusOK, Body: []byte("ok")}, nil
}

func (client *recordingClient) Shutdown(context.Context) error { return nil }

func testManifest(id string) tdriveplugin.Manifest {
	return tdriveplugin.Manifest{
		ID: id, Name: id, Version: "1.0.0", SDKVersion: "0.1",
		APIVersion: tdriveplugin.APIVersion, Author: "test", License: "MIT",
		RepositoryURL: "https://example.com/" + id,
		Events:        []string{"*"},
		Routes:        []tdriveplugin.RouteSpec{{Path: "/", Methods: []string{"GET"}, UI: true}},
	}
}

// installFor puts one account's installation of a plugin into the database and
// into the manager's active map, wired to a client the test can inspect.
func installFor(t *testing.T, db *database.DB, manager *Manager, owner database.User, id string) *recordingClient {
	t.Helper()
	manifest := testManifest(id)
	manifestJSON, _ := json.Marshal(manifest)
	record := database.PluginRecord{
		UserID: owner.ID, ID: id, Name: id, Version: "1.0.0", Author: "test",
		Enabled: true, Status: database.PluginStatusActive, Source: "release",
		BinaryDigest: strings.Repeat("a", 64),
		BinaryPath:   filepath.Join(t.TempDir(), id),
		ManifestJSON: string(manifestJSON),
		InstalledAt:  time.Now(), UpdatedAt: time.Now(),
	}
	if err := db.UpsertPlugin(context.Background(), record); err != nil {
		t.Fatalf("UpsertPlugin(%s/%s): %v", owner.Username, id, err)
	}
	client := &recordingClient{}
	manager.active[keyOf(record)] = &activePlugin{record: record, manifest: manifest, client: client}
	return client
}

func newOwnershipManager(t *testing.T) (*Manager, *database.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	t.Cleanup(func() { manager.Close(ctx) })
	return manager, db
}

// Two accounts can install the same plugin id. One account's browser must not
// be able to reach the other's child process through the shared route prefix.
func TestPluginRouteResolvesAgainstTheCallingAccount(t *testing.T) {
	ctx := context.Background()
	manager, db := newOwnershipManager(t)
	alice := newTestUser(t, db, "alice")
	bob := newTestUser(t, db, "bob")
	installFor(t, db, manager, alice, "shared")

	call := func(caller string) int {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/plugins/shared/", nil).
			WithContext(tdriveplugin.WithUserID(ctx, caller))
		manager.servePublicHTTP(recorder, request)
		return recorder.Code
	}

	if code := call(alice.ID); code != http.StatusOK {
		t.Fatalf("the owner got %d for their own plugin, want 200", code)
	}
	// 404 rather than 403: "not yours" and "does not exist" must look the same,
	// or the route becomes a way to enumerate what other people installed.
	if code := call(bob.ID); code != http.StatusNotFound {
		t.Fatalf("another account reached the plugin and got %d, want 404", code)
	}
	if code := call(""); code != http.StatusNotFound {
		t.Fatalf("an unauthenticated caller got %d, want 404", code)
	}
}

// A plugin sees its owner's API traffic and nobody else's. Without this,
// "install a plugin" would mean "read everyone's requests".
func TestHooksOnlyReachTheRequestingAccountsPlugins(t *testing.T) {
	ctx := context.Background()
	manager, db := newOwnershipManager(t)
	alice := newTestUser(t, db, "alice")
	bob := newTestUser(t, db, "bob")
	aliceClient := installFor(t, db, manager, alice, "watcher")
	bobClient := installFor(t, db, manager, bob, "watcher")

	operation := tdriveplugin.Operation{Name: "files.list", UserID: alice.ID}
	if _, err := manager.Before(ctx, operation); err != nil {
		t.Fatalf("Before: %v", err)
	}
	manager.After(ctx, operation)

	if aliceClient.before != 1 || aliceClient.after != 1 {
		t.Fatalf("the owner's plugin saw before=%d after=%d, want 1 and 1", aliceClient.before, aliceClient.after)
	}
	if bobClient.before != 0 || bobClient.after != 0 {
		t.Fatalf("another account's plugin observed the request: before=%d after=%d", bobClient.before, bobClient.after)
	}

	// An operation with no authenticated user belongs to no account, so it
	// reaches nobody. Background maintenance losing its hooks is the accepted
	// cost of not having a "sees everything" tier.
	if _, err := manager.Before(ctx, tdriveplugin.Operation{Name: "index.rebuild"}); err != nil {
		t.Fatalf("Before without a user: %v", err)
	}
	if aliceClient.before != 1 || bobClient.before != 0 {
		t.Fatalf("an unattributed operation was dispatched: alice=%d bob=%d", aliceClient.before, bobClient.before)
	}
}

// A tree event carries a path. Broadcasting one that was published without a
// user id would tell every account's plugin about somebody else's directories.
func TestUnattributedEventsOnlyBroadcastDeploymentFacts(t *testing.T) {
	manager, db := newOwnershipManager(t)
	alice := newTestUser(t, db, "alice")
	client := installFor(t, db, manager, alice, "listener")

	manager.dispatchEvent([]byte(`{"type":"tree","data":{"path":"/private"},"at":0}`))
	if client.events != 0 {
		t.Fatalf("an unattributed tree event reached %d plugins, want 0", client.events)
	}

	manager.dispatchEvent([]byte(`{"type":"telegram","data":{},"at":0}`))
	if client.events != 1 {
		t.Fatalf("a deployment-wide telegram event reached %d plugins, want 1", client.events)
	}

	manager.dispatchEvent([]byte(`{"type":"tree","data":{"path":"/mine"},"at":0,"userId":"` + alice.ID + `"}`))
	if client.events != 2 {
		t.Fatalf("the owner's own tree event did not reach their plugin: %d", client.events)
	}
}

// The per-account cap is what keeps the process budget bounded once every
// account can install for itself.
func TestInstallIsRefusedAtThePerAccountCap(t *testing.T) {
	ctx := context.Background()
	manager, db := newOwnershipManager(t)
	manager.cfg.Plugins.MaxPerUser = 1
	alice := newTestUser(t, db, "alice")
	bob := newTestUser(t, db, "bob")
	installFor(t, db, manager, alice, "first")

	if err := manager.checkInstallLimits(ctx, alice.ID); err == nil {
		t.Fatal("an account past its plugin allowance was allowed to install another")
	}
	// The cap is per account, so somebody else's installations do not count
	// against this one.
	if err := manager.checkInstallLimits(ctx, bob.ID); err != nil {
		t.Fatalf("another account was blocked by someone else's allowance: %v", err)
	}
}
