package tgc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/session"
)

func validSession(t *testing.T, authKey []byte) []byte {
	t.Helper()
	ctx := context.Background()
	storage := &session.StorageMemory{}
	loader := session.Loader{Storage: storage}
	if err := loader.Save(ctx, &session.Data{
		AuthKey:   authKey,
		AuthKeyID: []byte("auth-key-id"),
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	data, err := storage.Bytes(nil)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	return data
}

func TestValidateSession(t *testing.T) {
	ctx := context.Background()
	if err := validateSession(ctx, validSession(t, []byte("auth-key"))); err != nil {
		t.Fatalf("valid session rejected: %v", err)
	}

	for name, data := range map[string][]byte{
		"empty":       nil,
		"malformed":   []byte("not json"),
		"without key": validSession(t, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSession(ctx, data); err == nil {
				t.Fatal("invalid session accepted")
			}
		})
	}

	tooLarge := make([]byte, maxSessionSize+1)
	if err := validateSession(ctx, tooLarge); err == nil {
		t.Fatal("oversized session accepted")
	}
}

func TestReplaceSessionFileIsAtomicAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "session.json")
	want := validSession(t, []byte("auth-key"))
	if err := replaceSessionFile(path, want); err != nil {
		t.Fatalf("replaceSessionFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replaced session: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("replaced session contents differ")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat replaced session: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("session mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read session directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".session-import-") {
			t.Errorf("temporary session file %q was left behind", entry.Name())
		}
	}
}
