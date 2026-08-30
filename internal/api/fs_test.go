package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
)

func renameTestServer(t *testing.T) (*Server, *database.DB, database.User) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "fs.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{}
	service := drive.New(cfg, db, nil, zap.NewNop())
	server := &Server{
		cfg: cfg, db: db, drive: service, events: events.NewBroker(), log: zap.NewNop(),
	}
	return server, db, database.User{ID: "admin", Role: database.RoleAdmin}
}

func insertRenameFile(t *testing.T, db *database.DB, name string) string {
	t.Helper()
	id := database.NewID()
	if err := db.InsertFile(context.Background(), database.File{
		ID:           id,
		Name:         name,
		Size:         1,
		SegmentSize:  1,
		SegmentCount: 1,
		Status:       database.StatusComplete,
	}); err != nil {
		t.Fatalf("InsertFile %q: %v", name, err)
	}
	return id
}

func callBatchRename(t *testing.T, server *Server, user database.User, items []batchRenameItem) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(batchRenameRequest{Items: items})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/fs/batch-rename", bytes.NewReader(body))
	req = req.WithContext(auth.WithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	server.handleBatchRename(rec, req)
	return rec
}

func TestBatchRenameMovesChainsWithoutCollisions(t *testing.T) {
	server, db, user := renameTestServer(t)
	aID := insertRenameFile(t, db, "A")
	bID := insertRenameFile(t, db, "B")

	rec := callBatchRename(t, server, user, []batchRenameItem{
		{Path: "/A", Name: "B"},
		{Path: "/B", Name: "C"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch rename status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	a, err := db.FileByID(context.Background(), aID)
	if err != nil || a.Name != "B" {
		t.Fatalf("A after chain = (%q, %v), want B", a.Name, err)
	}
	b, err := db.FileByID(context.Background(), bID)
	if err != nil || b.Name != "C" {
		t.Fatalf("B after chain = (%q, %v), want C", b.Name, err)
	}
}

func TestBatchRenamePreflightsSourcesAndExternalTargets(t *testing.T) {
	server, db, user := renameTestServer(t)
	insertRenameFile(t, db, "A")
	insertRenameFile(t, db, "B")

	rec := callBatchRename(t, server, user, []batchRenameItem{
		{Path: "/A", Name: "renamed"},
		{Path: "/stale", Name: "later"},
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("stale source unexpectedly succeeded: %s", rec.Body.String())
	}
	if _, err := db.FileInDir(context.Background(), "", "A"); err != nil {
		t.Fatalf("A was changed before stale-source validation: %v", err)
	}

	rec = callBatchRename(t, server, user, []batchRenameItem{{Path: "/A", Name: "B"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("external target status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, err := db.FileInDir(context.Background(), "", "A"); err != nil {
		t.Fatalf("A changed after external-target validation: %v", err)
	}
}
