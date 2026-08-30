package plugin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

type fakeSourceBuilder struct {
	inspectResult BuilderResponse
	inspectCalls  int
	closeCalls    int
}

func (builder *fakeSourceBuilder) Inspect(context.Context, BuilderRequest) (BuilderResponse, error) {
	builder.inspectCalls++
	return builder.inspectResult, nil
}

func (builder *fakeSourceBuilder) Build(context.Context, BuilderRequest) (BuilderResponse, error) {
	return BuilderResponse{}, nil
}

func (builder *fakeSourceBuilder) Close() { builder.closeCalls++ }

func TestManagerHasNoPluginRuntimeWithoutInstalledPlugins(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	cfg := &config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}
	manager := New(cfg, db, nil, nil, nil, nil, zap.NewNop())
	fakeBuilder := &fakeSourceBuilder{}
	manager.SetBuilder(fakeBuilder)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if manager.HasHooks() {
		t.Fatal("manager reported hooks without installed plugins")
	}
	if fakeBuilder.inspectCalls != 0 {
		t.Fatalf("startup unexpectedly inspected source %d times", fakeBuilder.inspectCalls)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fakeBuilder.closeCalls != 1 {
		t.Fatalf("builder was closed %d times, want 1", fakeBuilder.closeCalls)
	}
}

func TestInspectionIsSingleUse(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	manifest := tdriveplugin.Manifest{
		ID:            "example",
		Name:          "Example",
		Version:       "1.0.0",
		SDKVersion:    "0.1",
		APIVersion:    tdriveplugin.APIVersion,
		Author:        "Example",
		License:       "MIT",
		RepositoryURL: "https://example.com/plugin",
		Entrypoint:    "./cmd/plugin",
	}
	fakeBuilder := &fakeSourceBuilder{inspectResult: BuilderResponse{
		Manifest: manifest, SourceDigest: "source-digest",
	}}
	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	manager.SetBuilder(fakeBuilder)

	inspection, err := manager.Inspect(ctx, manifest.RepositoryURL, "v1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.ID == "" || !inspection.Compatible || inspection.ExpiresAt.Before(time.Now()) {
		t.Fatalf("invalid inspection: %+v", inspection)
	}
	consumed, err := manager.consumeInspection(inspection.ID)
	if err != nil {
		t.Fatalf("consumeInspection: %v", err)
	}
	if consumed.ID != inspection.ID {
		t.Fatalf("consumed the wrong inspection: %+v", consumed)
	}
	if _, err := manager.consumeInspection(inspection.ID); err == nil {
		t.Fatal("inspection was reusable")
	}
}
