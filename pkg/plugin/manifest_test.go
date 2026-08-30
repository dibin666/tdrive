package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func validTestManifest() Manifest {
	return Manifest{
		ID:            "example-plugin",
		Name:          "Example",
		Version:       "1.2.3",
		SDKVersion:    "0.1",
		APIVersion:    APIVersion,
		Author:        "Example",
		License:       "MIT",
		RepositoryURL: "https://github.com/example/tdrive-plugin",
		Entrypoint:    "./cmd/plugin",
	}
}

func TestManifestValidate(t *testing.T) {
	manifest := validTestManifest()
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	tests := map[string]func(*Manifest){
		"invalid id": func(value *Manifest) { value.ID = "../plugin" },
		"invalid version": func(value *Manifest) { value.Version = "1.2" },
		"missing repository": func(value *Manifest) { value.RepositoryURL = "" },
		"non HTTPS documentation": func(value *Manifest) { value.DocumentationURL = "http://example.com/docs" },
		"absolute entrypoint": func(value *Manifest) { value.Entrypoint = "/cmd/plugin" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			invalid := validTestManifest()
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}

func TestReadManifest(t *testing.T) {
	root := t.TempDir()
	manifest := validTestManifest()
	data := []byte(`{
  "id": "example-plugin",
  "name": "Example",
  "version": "1.2.3",
  "sdkVersion": "0.1",
  "apiVersion": 1,
  "author": "Example",
  "license": "MIT",
  "repositoryUrl": "https://github.com/example/tdrive-plugin",
  "entrypoint": "./cmd/plugin"
}`)
	if err := os.WriteFile(filepath.Join(root, ManifestFile), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	got, err := ReadManifest(root)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ID != manifest.ID || got.Version != manifest.Version || got.Entrypoint != manifest.Entrypoint {
		t.Fatalf("manifest changed while reading: %+v", got)
	}
}

