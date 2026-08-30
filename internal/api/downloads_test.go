package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
)

func downloadTestServer(t *testing.T) (*Server, *database.DB, database.User) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "downloads.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.Config{}
	server := &Server{
		cfg: cfg, db: db, drive: drive.New(cfg, db, nil, zap.NewNop()),
		events: events.NewBroker(), log: zap.NewNop(),
	}
	// download_jobs.user_id is a foreign key, so the owner has to be real.
	user, err := db.CreateUser(context.Background(), "owner", "hash", database.RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return server, db, user
}

func withRouteID(req *http.Request, id string) *http.Request {
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", id)
	routeCtx.URLParams.Add("kind", "download")
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
}

// A direct download runs in the browser, so the server has no worker to stop —
// but it still owns the row. Refusing to cancel one leaves it "in progress"
// forever, and since only finished transfers can be deleted, the history row
// becomes impossible to get rid of. That is what happens to every transfer
// whose tab was closed mid-download.
func TestCancelDownloadEndsClientDrivenTransfers(t *testing.T) {
	for _, mode := range []database.DownloadMode{database.DownloadDirect, database.DownloadSegments} {
		t.Run(string(mode), func(t *testing.T) {
			server, db, user := downloadTestServer(t)
			job := database.DownloadJob{
				ID:        database.NewID(),
				UserID:    user.ID,
				Name:      "stuck.mkv",
				TotalSize: 1 << 20,
				Mode:      mode,
				Status:    database.DownloadRunning,
			}
			if err := db.InsertDownload(context.Background(), job); err != nil {
				t.Fatalf("InsertDownload: %v", err)
			}

			req := httptest.NewRequest(http.MethodDelete, "/downloads/"+job.ID, nil)
			req = withRouteID(req.WithContext(auth.WithUser(req.Context(), user)), job.ID)
			rec := httptest.NewRecorder()
			server.handleCancelDownload(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("cancel status = %d, want 204: %s", rec.Code, rec.Body.String())
			}

			fresh, err := db.DownloadByID(context.Background(), job.ID)
			if err != nil {
				t.Fatalf("DownloadByID: %v", err)
			}
			if fresh.Status != database.DownloadCancelled {
				t.Fatalf("status after cancel = %q, want cancelled", fresh.Status)
			}

			// The point of cancelling is being able to clear the row afterwards.
			del := httptest.NewRequest(http.MethodDelete, "/transfers/download/"+job.ID, nil)
			del = withRouteID(del.WithContext(auth.WithUser(del.Context(), user)), job.ID)
			delRec := httptest.NewRecorder()
			server.handleDeleteTransfer(delRec, del)
			if delRec.Code != http.StatusNoContent {
				t.Fatalf("delete status = %d, want 204: %s", delRec.Code, delRec.Body.String())
			}
			if _, err := db.DownloadByID(context.Background(), job.ID); err == nil {
				t.Fatal("the cancelled download row survived deletion")
			}
		})
	}
}

// Cancelling something that already finished is a no-op, not an error: the
// browser may have reported completion just as the user reached for the button.
func TestCancelDownloadLeavesTerminalRowsAlone(t *testing.T) {
	server, db, user := downloadTestServer(t)
	job := database.DownloadJob{
		ID:     database.NewID(),
		UserID: user.ID,
		Name:   "done.bin",
		Mode:   database.DownloadDirect,
		Status: database.DownloadComplete,
	}
	if err := db.InsertDownload(context.Background(), job); err != nil {
		t.Fatalf("InsertDownload: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/downloads/"+job.ID, nil)
	req = withRouteID(req.WithContext(auth.WithUser(req.Context(), user)), job.ID)
	rec := httptest.NewRecorder()
	server.handleCancelDownload(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	fresh, err := db.DownloadByID(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("DownloadByID: %v", err)
	}
	if fresh.Status != database.DownloadComplete {
		t.Fatalf("status = %q, want it left complete", fresh.Status)
	}
}
