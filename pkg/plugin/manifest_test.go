package plugin

import (
	"strings"
	"testing"
)

const testArtifactDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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
		Artifacts: map[string]Artifact{
			"linux/amd64": {
				URL:    "https://github.com/example/tdrive-plugin/releases/download/v1.2.3/plugin-linux-amd64",
				SHA256: testArtifactDigest,
			},
		},
	}
}

func TestManifestValidate(t *testing.T) {
	manifest := validTestManifest()
	if err := manifest.ValidatePublished(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	// The entrypoint only documents how the author builds the plugin, so an
	// absent one must stay installable.
	withoutEntrypoint := validTestManifest()
	withoutEntrypoint.Entrypoint = ""
	if err := withoutEntrypoint.ValidatePublished(); err != nil {
		t.Fatalf("manifest without an entrypoint rejected: %v", err)
	}

	// A running plugin reports its manifest over RPC and cannot state the
	// digest of its own executable, so Validate must not demand artifacts.
	reported := validTestManifest()
	reported.Artifacts = nil
	if err := reported.Validate(); err != nil {
		t.Fatalf("self-reported manifest without artifacts rejected: %v", err)
	}

	tests := map[string]func(*Manifest){
		"invalid id":              func(value *Manifest) { value.ID = "../plugin" },
		"invalid version":         func(value *Manifest) { value.Version = "1.2" },
		"missing repository":      func(value *Manifest) { value.RepositoryURL = "" },
		"non HTTPS documentation": func(value *Manifest) { value.DocumentationURL = "http://example.com/docs" },
		"absolute entrypoint":     func(value *Manifest) { value.Entrypoint = "/cmd/plugin" },
		"no artifacts":            func(value *Manifest) { value.Artifacts = nil },
		"empty artifacts":         func(value *Manifest) { value.Artifacts = map[string]Artifact{} },
		"malformed platform": func(value *Manifest) {
			value.Artifacts = map[string]Artifact{
				"linux-amd64": {URL: "https://example.com/plugin", SHA256: testArtifactDigest},
			}
		},
		"non HTTPS artifact": func(value *Manifest) {
			value.Artifacts = map[string]Artifact{
				"linux/amd64": {URL: "http://example.com/plugin", SHA256: testArtifactDigest},
			}
		},
		"short artifact digest": func(value *Manifest) {
			value.Artifacts = map[string]Artifact{
				"linux/amd64": {URL: "https://example.com/plugin", SHA256: "abc123"},
			}
		},
		"uppercase artifact digest": func(value *Manifest) {
			value.Artifacts = map[string]Artifact{
				"linux/amd64": {URL: "https://example.com/plugin", SHA256: "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"},
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			invalid := validTestManifest()
			mutate(&invalid)
			if err := invalid.ValidatePublished(); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}

func TestArtifactForNamesThePublishedPlatforms(t *testing.T) {
	manifest := validTestManifest()
	manifest.Artifacts["linux/arm64"] = Artifact{
		URL:    "https://github.com/example/tdrive-plugin/releases/download/v1.2.3/plugin-linux-arm64",
		SHA256: testArtifactDigest,
	}

	artifact, err := manifest.ArtifactFor("linux", "arm64")
	if err != nil {
		t.Fatalf("ArtifactFor(linux, arm64): %v", err)
	}
	if artifact.URL != manifest.Artifacts["linux/arm64"].URL {
		t.Fatalf("ArtifactFor returned the wrong artifact: %+v", artifact)
	}

	// An administrator on an unsupported platform can only act on the error if
	// it says which platforms do exist.
	_, err = manifest.ArtifactFor("windows", "amd64")
	if err == nil {
		t.Fatal("ArtifactFor accepted a platform the plugin does not publish")
	}
	for _, want := range []string{"windows/amd64", "linux/amd64", "linux/arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ArtifactFor error %q does not mention %q", err, want)
		}
	}
}

func TestParseManifest(t *testing.T) {
	data := []byte(`{
  "id": "example-plugin",
  "name": "Example",
  "version": "1.2.3",
  "sdkVersion": "0.1",
  "apiVersion": 1,
  "author": "Example",
  "license": "MIT",
  "repositoryUrl": "https://github.com/example/tdrive-plugin",
  "entrypoint": "./cmd/plugin",
  "artifacts": {
    "linux/amd64": {
      "url": "https://github.com/example/tdrive-plugin/releases/download/v1.2.3/plugin-linux-amd64",
      "sha256": "` + testArtifactDigest + `"
    }
  }
}`)
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	want := validTestManifest()
	if got.ID != want.ID || got.Version != want.Version || got.Entrypoint != want.Entrypoint {
		t.Fatalf("manifest changed while parsing: %+v", got)
	}
	if got.Artifacts["linux/amd64"] != want.Artifacts["linux/amd64"] {
		t.Fatalf("artifact changed while parsing: %+v", got.Artifacts)
	}

	// A manifest that parses but does not validate must not reach the caller.
	if _, err := ParseManifest([]byte(`{"id":"example-plugin","name":"Example"}`)); err == nil {
		t.Fatal("ParseManifest accepted a manifest with no artifacts")
	}
}
