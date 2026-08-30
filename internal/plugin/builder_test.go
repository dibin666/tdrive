package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateSourceURL(t *testing.T) {
	valid := []string{
		"https://github.com/example/tdrive-plugin",
		"git::https://git.example.com/tdrive-plugin.git",
		"https://example.com/releases/plugin.tar.gz",
	}
	for _, source := range valid {
		if _, err := ValidateSourceURL(source); err != nil {
			t.Errorf("ValidateSourceURL(%q): %v", source, err)
		}
	}

	invalid := []string{
		"http://github.com/example/tdrive-plugin",
		"file:///tmp/plugin",
		"git::ssh://git@example.com/plugin",
		"https://user:password@example.com/plugin",
		"https://127.0.0.1/plugin",
	}
	for _, source := range invalid {
		if _, err := ValidateSourceURL(source); err == nil {
			t.Errorf("ValidateSourceURL(%q) accepted an unsafe source", source)
		}
	}
}

func TestValidateRef(t *testing.T) {
	for _, ref := range []string{"", "v1.0.0", "0123456789abcdef", "feature/plugin"} {
		if err := ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q): %v", ref, err)
		}
	}
	for _, ref := range []string{"-o", "main..broken", "feature~1", "feature?query"} {
		if err := ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) accepted an unsafe ref", ref)
		}
	}
}

func TestSourceWithRefDefaultsRepositoryURLsToGit(t *testing.T) {
	tests := map[string]struct {
		source string
		ref    string
		want   string
	}{
		"repository": {
			source: "https://github.com/example/plugin",
			ref:    "v1.0.0",
			want:   "git::https://github.com/example/plugin?ref=v1.0.0",
		},
		"explicit git": {
			source: "git::https://github.com/example/plugin",
			ref:    "main",
			want:   "git::https://github.com/example/plugin?ref=main",
		},
		"archive": {
			source: "https://example.com/plugin.tar.gz",
			ref:    "",
			want:   "https://example.com/plugin.tar.gz",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := sourceWithRef(test.source, test.ref); got != test.want {
				t.Fatalf("sourceWithRef(%q, %q) = %q, want %q", test.source, test.ref, got, test.want)
			}
		})
	}
}

func TestDigestSourceIsStableAndBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := digestSource(root, 1024)
	if err != nil {
		t.Fatalf("first digest: %v", err)
	}
	second, err := digestSource(root, 1024)
	if err != nil {
		t.Fatalf("second digest: %v", err)
	}
	if first != second {
		t.Fatalf("digest changed between identical reads: %s != %s", first, second)
	}
	if _, err := digestSource(root, 1); err == nil {
		t.Fatal("source over the size limit was accepted")
	}
}
