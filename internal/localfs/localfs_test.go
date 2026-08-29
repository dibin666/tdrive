package localfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestListAndOpen(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	service := New(root)
	entries, clean, err := service.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if clean != "/" {
		t.Fatalf("clean path = %q, want /", clean)
	}
	if len(entries) != 2 || !entries[0].IsDir || entries[0].Path != "/nested" {
		t.Fatalf("entries = %#v, want nested directory followed by hello.txt", entries)
	}

	file, entry, err := service.Open("/hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" || entry.Size != 5 {
		t.Fatalf("opened file = %q, size %d", data, entry.Size)
	}
}

func TestRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	service := New(root)
	for _, path := range []string{"../secret.txt", "/../secret.txt", "/nested/../../secret.txt", `/nested\\secret.txt`} {
		if _, _, err := service.List(path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("List(%q) error = %v, want ErrInvalidPath", path, err)
		}
	}
	if _, _, err := service.Open("/link.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open symlink error = %v, want ErrNotFound", err)
	}
	entries, _, err := service.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("symlink was listed: %#v", entries)
	}
}

func TestDisabledService(t *testing.T) {
	if _, _, err := New("").List("/"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("List on disabled service error = %v, want ErrDisabled", err)
	}
}
