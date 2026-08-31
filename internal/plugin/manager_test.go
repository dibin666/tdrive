package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

type fakeReleaseFetcher struct {
	manifest      tdriveplugin.Manifest
	digest        string
	manifestCalls int
	downloadCalls int
}

func (fetcher *fakeReleaseFetcher) Manifest(context.Context, string) (tdriveplugin.Manifest, string, error) {
	fetcher.manifestCalls++
	return fetcher.manifest, fetcher.digest, nil
}

func (fetcher *fakeReleaseFetcher) Download(context.Context, tdriveplugin.Artifact, string) (string, error) {
	fetcher.downloadCalls++
	return "", nil
}

func TestManagerHasNoPluginRuntimeWithoutInstalledPlugins(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	cfg := &config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}
	manager := New(cfg, db, nil, nil, nil, nil, zap.NewNop())
	fetcher := &fakeReleaseFetcher{}
	manager.SetFetcher(fetcher)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if manager.HasHooks() {
		t.Fatal("manager reported hooks without installed plugins")
	}
	if fetcher.manifestCalls != 0 || fetcher.downloadCalls != 0 {
		t.Fatalf("startup unexpectedly reached the network: %d manifest, %d download",
			fetcher.manifestCalls, fetcher.downloadCalls)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestInspectionIsSingleUse(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	artifact := tdriveplugin.Artifact{
		URL:    "https://example.com/plugin/example-" + tdriveplugin.HostPlatform(),
		SHA256: strings.Repeat("a", 64),
	}
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
		Artifacts:     map[string]tdriveplugin.Artifact{tdriveplugin.HostPlatform(): artifact},
	}
	fetcher := &fakeReleaseFetcher{manifest: manifest, digest: "manifest-digest"}
	manager := New(&config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}, db, nil, nil, nil, nil, zap.NewNop())
	manager.SetFetcher(fetcher)

	inspection, err := manager.Inspect(ctx, "https://example.com/plugin/tdrive.plugin.json")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.ID == "" || !inspection.Compatible || inspection.ExpiresAt.Before(time.Now()) {
		t.Fatalf("invalid inspection: %+v", inspection)
	}
	// The inspection must carry the exact binary the confirmation screen
	// described, so Install never has to consult the manifest again.
	if inspection.BinaryURL != artifact.URL || inspection.BinaryDigest != artifact.SHA256 {
		t.Fatalf("inspection did not pin the host artifact: %+v", inspection)
	}
	if fetcher.downloadCalls != 0 {
		t.Fatalf("inspection downloaded %d binaries, want 0", fetcher.downloadCalls)
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

// TestInstallRejectsABinaryThatDoesNotMatchTheManifest drives Inspect and
// Install through the real HTTP fetcher, including the production
// ValidateDownloadURL guard, and asserts that a served payload which does not
// hash to the declared digest installs nothing at all.
func TestInstallRejectsABinaryThatDoesNotMatchTheManifest(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const declaredDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	base := "https://tdrive.test"
	mux := http.NewServeMux()
	mux.HandleFunc("/plugin", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not the binary the manifest promised"))
	})
	mux.HandleFunc("/tdrive.plugin.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id":"example","name":"Example","version":"1.0.0",
			"sdkVersion":"0.1","apiVersion":%d,"author":"Example","license":"MIT",
			"repositoryUrl":"https://example.com/plugin",
			"artifacts":{%q:{"url":"%s/plugin","sha256":%q}}}`,
			tdriveplugin.APIVersion, tdriveplugin.HostPlatform(), base, declaredDigest)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()

	pluginDir := t.TempDir()
	manager := New(&config.Config{Plugins: config.Plugins{Dir: pluginDir}}, db, nil, nil, nil, nil, zap.NewNop())
	manager.SetFetcher(testServerFetcher(server))
	defer manager.Close(ctx)

	inspection, err := manager.Inspect(ctx, base+"/tdrive.plugin.json")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspection.BinaryDigest != declaredDigest {
		t.Fatalf("inspection pinned %s, want the manifest digest", inspection.BinaryDigest)
	}
	if _, err := manager.Install(ctx, inspection.ID); err == nil {
		t.Fatal("a binary that did not match the manifest digest was installed")
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "example")); !os.IsNotExist(err) {
		t.Fatalf("the rejected install left a binary behind: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(pluginDir, ".staging")); len(entries) != 0 {
		t.Fatalf("the rejected install left %d staging entries behind", len(entries))
	}
	if _, err := db.PluginByID(ctx, "example"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("the rejected install recorded metadata: %v", err)
	}
}

// testServerFetcher keeps the production ValidateDownloadURL guard — so the
// manifest and artifact URLs really are public-looking HTTPS ones — and only
// redirects the dial to the local TLS server.
func testServerFetcher(server *httptest.Server) *httpFetcher {
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	address := strings.TrimPrefix(server.URL, "https://")
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	return &httpFetcher{
		client:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
		validate: ValidateDownloadURL,
		maxBytes: config.DefaultPluginBinaryLimit,
	}
}
