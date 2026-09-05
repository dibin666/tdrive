package plugin

import (
	"context"
	"encoding/json"
	"errors"
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

type failingHTTPClient struct{}

func (failingHTTPClient) Before(context.Context, tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	return tdriveplugin.OperationResult{Allowed: true}, nil
}

func (failingHTTPClient) After(context.Context, tdriveplugin.Operation) error { return nil }

func (failingHTTPClient) OnEvent(context.Context, tdriveplugin.Event) error { return nil }

func (failingHTTPClient) HandleHTTP(context.Context, tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error) {
	return tdriveplugin.HTTPResponse{}, errors.New("simulated plugin RPC failure")
}

func (failingHTTPClient) Shutdown(context.Context) error { return nil }

func TestPluginHTTPFailureDoesNotTurnTheRouteInto404(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	owner := newTestUser(t, db, "owner")

	manifest := tdriveplugin.Manifest{
		ID: "broken", Name: "Broken", Version: "1.0.0", SDKVersion: "0.1",
		APIVersion: tdriveplugin.APIVersion, Author: "test", License: "MIT",
		RepositoryURL: "https://example.com/broken", Routes: []tdriveplugin.RouteSpec{{Path: "/", Methods: []string{"GET"}, UI: true}},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := db.UpsertPlugin(ctx, database.PluginRecord{
		UserID: owner.ID, ID: "broken", Name: "Broken", Version: "1.0.0", Author: "test", Enabled: true,
		Status: database.PluginStatusActive, BinaryDigest: strings.Repeat("a", 64),
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"), ManifestJSON: string(manifestJSON),
		InstalledAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertPlugin: %v", err)
	}

	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	defer manager.Close(ctx)
	manager.active[pluginKey{userID: owner.ID, pluginID: "broken"}] = &activePlugin{
		record: database.PluginRecord{UserID: owner.ID, ID: "broken"}, manifest: manifest, client: failingHTTPClient{},
	}

	call := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/plugins/broken/", nil).
			WithContext(tdriveplugin.WithUserID(ctx, owner.ID))
		manager.servePublicHTTP(recorder, request)
		return recorder
	}
	first := call()
	if first.Code != http.StatusBadGateway {
		t.Fatalf("first failure status = %d, want 502", first.Code)
	}
	// The recovery goroutine may not have run yet; either way the active
	// placeholder must keep the route declared and reachable.
	second := call()
	if second.Code == http.StatusNotFound {
		t.Fatal("a transient plugin RPC failure removed the HTTP route")
	}
}

// blockingHTTPClient returns whatever the caller's context ended with, which is
// what the real RPC client does: it races the call against ctx.Done and hands
// back ctx.Err().
type blockingHTTPClient struct{}

func (blockingHTTPClient) Before(context.Context, tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	return tdriveplugin.OperationResult{Allowed: true}, nil
}

func (blockingHTTPClient) After(context.Context, tdriveplugin.Operation) error { return nil }

func (blockingHTTPClient) OnEvent(context.Context, tdriveplugin.Event) error { return nil }

func (blockingHTTPClient) HandleHTTP(ctx context.Context, _ tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error) {
	<-ctx.Done()
	return tdriveplugin.HTTPResponse{}, ctx.Err()
}

func (blockingHTTPClient) Shutdown(context.Context) error { return nil }

// A plugin page polls, so a browser cancels an in-flight plugin request every
// time somebody switches tabs, reloads, or navigates away. Treating that as a
// runtime failure restarted the child — and for a sync plugin, restarting the
// child threw away every transfer that was in flight.
func TestClientCancelledPluginRequestDoesNotRestartThePlugin(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	owner := newTestUser(t, db, "owner")

	manifest := tdriveplugin.Manifest{
		ID: "slow", Name: "Slow", Version: "1.0.0", SDKVersion: "0.1",
		APIVersion: tdriveplugin.APIVersion, Author: "test", License: "MIT",
		RepositoryURL: "https://example.com/slow", Routes: []tdriveplugin.RouteSpec{{Path: "/", Methods: []string{"GET"}, UI: true}},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := db.UpsertPlugin(ctx, database.PluginRecord{
		UserID: owner.ID, ID: "slow", Name: "Slow", Version: "1.0.0", Author: "test", Enabled: true,
		Status: database.PluginStatusActive, BinaryDigest: strings.Repeat("a", 64),
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"), ManifestJSON: string(manifestJSON),
		InstalledAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertPlugin: %v", err)
	}

	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	defer manager.Close(ctx)
	active := &activePlugin{
		record: database.PluginRecord{UserID: owner.ID, ID: "slow"}, manifest: manifest, client: blockingHTTPClient{},
	}
	manager.active[pluginKey{userID: owner.ID, pluginID: "slow"}] = active

	requestCtx, cancelRequest := context.WithCancel(tdriveplugin.WithUserID(ctx, owner.ID))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/plugins/slow/", nil).WithContext(requestCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.servePublicHTTP(recorder, request)
	}()
	cancelRequest()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled plugin request did not return")
	}

	if recorder.Code != clientClosedRequest {
		t.Fatalf("status = %d, want %d for a client that went away", recorder.Code, clientClosedRequest)
	}
	manager.mu.RLock()
	recovering := manager.recovering[pluginKey{userID: owner.ID, pluginID: "slow"}]
	failed := active.failed
	manager.mu.RUnlock()
	if recovering || failed {
		t.Fatal("a client-cancelled request was treated as a plugin failure and restarted the child")
	}

	record, err := db.PluginByID(ctx, owner.ID, "slow")
	if err != nil {
		t.Fatalf("PluginByID: %v", err)
	}
	if record.Status != database.PluginStatusActive {
		t.Fatalf("plugin status = %q, want it left active", record.Status)
	}
}

// A child that is alive but slower than pluginCallTimeout is reported to the
// browser, not killed: a plugin moving gigabytes can legitimately be busy, and
// restarting it is far more destructive than a 504.
func TestSlowPluginResponseTimesOutWithoutRestartingTheChild(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	owner := newTestUser(t, db, "owner")

	manifest := tdriveplugin.Manifest{
		ID: "busy", Name: "Busy", Version: "1.0.0", SDKVersion: "0.1",
		APIVersion: tdriveplugin.APIVersion, Author: "test", License: "MIT",
		RepositoryURL: "https://example.com/busy", Routes: []tdriveplugin.RouteSpec{{Path: "/", Methods: []string{"GET"}, UI: true}},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := db.UpsertPlugin(ctx, database.PluginRecord{
		UserID: owner.ID, ID: "busy", Name: "Busy", Version: "1.0.0", Author: "test", Enabled: true,
		Status: database.PluginStatusActive, BinaryDigest: strings.Repeat("a", 64),
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"), ManifestJSON: string(manifestJSON),
		InstalledAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertPlugin: %v", err)
	}

	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	defer manager.Close(ctx)
	active := &activePlugin{
		record: database.PluginRecord{UserID: owner.ID, ID: "busy"}, manifest: manifest, client: failingHTTPClient{},
	}
	manager.active[pluginKey{userID: owner.ID, pluginID: "busy"}] = active

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/plugins/busy/", nil)
	manager.reportPluginCallError(recorder, active, request, context.DeadlineExceeded)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", recorder.Code)
	}
	manager.mu.RLock()
	recovering := manager.recovering[pluginKey{userID: owner.ID, pluginID: "busy"}]
	failed := active.failed
	manager.mu.RUnlock()
	if recovering || failed {
		t.Fatal("a timeout against a live child restarted it")
	}
}
