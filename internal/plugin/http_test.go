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

	manifest := tdriveplugin.Manifest{
		ID: "broken", Name: "Broken", Version: "1.0.0", SDKVersion: "0.1",
		APIVersion: tdriveplugin.APIVersion, Author: "test", License: "MIT",
		RepositoryURL: "https://example.com/broken", Routes: []tdriveplugin.RouteSpec{{Path: "/", Methods: []string{"GET"}, UI: true}},
	}
	manifestJSON, _ := json.Marshal(manifest)
	if err := db.UpsertPlugin(ctx, database.PluginRecord{
		ID: "broken", Name: "Broken", Version: "1.0.0", Author: "test", Enabled: true,
		Status: database.PluginStatusActive, BinaryDigest: strings.Repeat("a", 64),
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"), ManifestJSON: string(manifestJSON),
		InstalledAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertPlugin: %v", err)
	}

	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	defer manager.Close(ctx)
	manager.active["broken"] = &activePlugin{
		record: database.PluginRecord{ID: "broken"}, manifest: manifest, client: failingHTTPClient{},
	}

	call := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/plugins/broken/", nil)
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
