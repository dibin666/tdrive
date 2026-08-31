package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// A plugin installed without the .exe suffix is unreachable on Windows:
// os/exec's findExecutable never stats an extension-less path, it only tries
// that path plus each PATHEXT entry, so Start fails with ErrNotFound before
// CreateProcess is reached.
func TestExecutableNameSuffixesWindowsBinaries(t *testing.T) {
	tests := []struct {
		name, goos, want string
	}{
		{"hello", "windows", "hello.exe"},
		{"plugin", "windows", "plugin.exe"},
		{"hello", "linux", "hello"},
		{"hello", "darwin", "hello"},
		{"hello", "freebsd", "hello"},
	}
	for _, test := range tests {
		if got := executableName(test.name, test.goos); got != test.want {
			t.Errorf("executableName(%q, %q) = %q, want %q", test.name, test.goos, got, test.want)
		}
	}
}

func TestValidateDownloadURL(t *testing.T) {
	valid := []string{
		"https://github.com/example/tdrive-plugin/releases/download/v1/tdrive.plugin.json",
		"https://raw.githubusercontent.com/example/tdrive-plugin/v1/tdrive.plugin.json",
		"https://example.com/plugins/hello-linux-amd64",
	}
	for _, source := range valid {
		if _, err := ValidateDownloadURL(source); err != nil {
			t.Errorf("ValidateDownloadURL(%q): %v", source, err)
		}
	}

	invalid := []string{
		"",
		"http://github.com/example/tdrive-plugin",
		"file:///tmp/plugin",
		"ssh://git@example.com/plugin",
		"https://user:password@example.com/plugin",
		"https://127.0.0.1/plugin",
		"https://10.0.0.5/plugin",
		"https://localhost/plugin",
		"https://registry.local/plugin",
	}
	for _, source := range invalid {
		if _, err := ValidateDownloadURL(source); err == nil {
			t.Errorf("ValidateDownloadURL(%q) accepted an unsafe URL", source)
		}
	}
}

// testFetcher points an httpFetcher at a local test server. ValidateDownloadURL
// rejects loopback addresses, so the URL guard is exercised separately above
// and the transport is exercised here with a permissive validator.
func testFetcher(t *testing.T, handler http.Handler, maxBytes int64) (*httpFetcher, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return newFetcher(url.Parse, maxBytes, 10*time.Second), server.URL
}

func manifestJSON(digest string) string {
	return fmt.Sprintf(`{
  "id": "hello",
  "name": "Hello",
  "version": "0.1.0",
  "sdkVersion": "0.1",
  "apiVersion": 1,
  "author": "tdrive",
  "license": "MIT",
  "repositoryUrl": "https://github.com/example/tdrive-plugin",
  "artifacts": {
    "%s": {"url": "https://example.com/hello", "sha256": "%s"}
  }
}`, tdriveplugin.HostPlatform(), digest)
}

func TestFetchManifestReturnsTheDigestOfTheBytesItRead(t *testing.T) {
	body := manifestJSON(strings.Repeat("a", 64))
	fetcher, base := testFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}), 1<<20)

	manifest, digest, err := fetcher.Manifest(context.Background(), base+"/tdrive.plugin.json")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if manifest.ID != "hello" || manifest.Version != "0.1.0" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	want := sha256.Sum256([]byte(body))
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("manifest digest = %s, want %s", digest, hex.EncodeToString(want[:]))
	}
	if _, err := manifest.ArtifactFor("linux", "amd64"); err != nil && tdriveplugin.HostPlatform() == "linux/amd64" {
		t.Fatalf("ArtifactFor: %v", err)
	}
}

func TestFetchManifestRejectsAnInvalidDocument(t *testing.T) {
	fetcher, base := testFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Valid JSON, but no artifacts: nothing here is installable.
		_, _ = w.Write([]byte(`{"id":"hello","name":"Hello","version":"0.1.0"}`))
	}), 1<<20)

	if _, _, err := fetcher.Manifest(context.Background(), base+"/tdrive.plugin.json"); err == nil {
		t.Fatal("Manifest accepted a document with no artifacts")
	}
}

func TestDownloadWritesTheBinaryOnlyWhenTheDigestMatches(t *testing.T) {
	payload := []byte("#!/bin/true\nplugin binary\n")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	fetcher, base := testFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}), 1<<20)

	destination := filepath.Join(t.TempDir(), "staging", "plugin")
	got, err := fetcher.Download(context.Background(),
		tdriveplugin.Artifact{URL: base + "/plugin", SHA256: digest}, destination)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got != digest {
		t.Fatalf("Download digest = %s, want %s", got, digest)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat downloaded plugin: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("downloaded plugin is not executable: %s", info.Mode())
	}

	// A manifest that declares the wrong digest must leave nothing behind, or
	// the next install would find a stale staging file.
	tampered := filepath.Join(t.TempDir(), "staging", "plugin")
	if _, err := fetcher.Download(context.Background(),
		tdriveplugin.Artifact{URL: base + "/plugin", SHA256: strings.Repeat("b", 64)}, tampered); err == nil {
		t.Fatal("Download accepted a binary that did not match the manifest digest")
	}
	if _, err := os.Stat(tampered); !os.IsNotExist(err) {
		t.Fatalf("a mismatched download was left on disk: %v", err)
	}
}

func TestDownloadRejectsABinaryOverTheSizeLimit(t *testing.T) {
	payload := make([]byte, 4096)
	sum := sha256.Sum256(payload)
	fetcher, base := testFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}), 1024)

	destination := filepath.Join(t.TempDir(), "plugin")
	_, err := fetcher.Download(context.Background(),
		tdriveplugin.Artifact{URL: base + "/plugin", SHA256: hex.EncodeToString(sum[:])}, destination)
	if err == nil {
		t.Fatal("Download accepted a binary over the size limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Download error does not mention the limit: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("an oversized download was left on disk: %v", err)
	}
}

func TestDownloadReportsAnHTTPFailure(t *testing.T) {
	fetcher, base := testFetcher(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}), 1<<20)

	destination := filepath.Join(t.TempDir(), "plugin")
	_, err := fetcher.Download(context.Background(),
		tdriveplugin.Artifact{URL: base + "/plugin", SHA256: strings.Repeat("a", 64)}, destination)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Download error = %v, want an HTTP 404", err)
	}
}
