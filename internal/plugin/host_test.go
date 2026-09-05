package plugin

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// A cancelled plugin upload must wake both sides of the brokered stream. An
// io.Pipe does not observe context cancellation on its own, so leaving it open
// makes the plugin's retry action race an upload goroutine that never returns.
func TestPluginAbortUploadStopsRegisteredWork(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	job := database.UploadJob{
		ID: "job-plugin-cancel", Name: "cancel.bin", SegmentCount: 1,
		DoneMask: database.NewMask(1), Status: database.JobPending,
	}
	if err := db.InsertJob(ctx, job); err != nil {
		t.Fatalf("InsertJob: %v", err)
	}
	cfg := &config.Config{Plugins: config.Plugins{Dir: t.TempDir()}}
	driveSvc := drive.New(cfg, db, nil, zap.NewNop())
	workerCtx, release := driveSvc.WatchUploadJob(ctx, job.ID)
	// The real stream worker unregisters itself after observing cancellation.
	go func() {
		<-workerCtx.Done()
		release()
	}()

	manager := New(cfg, db, nil, driveSvc, nil, nil, zap.NewNop())
	defer manager.Close(ctx)
	host := &managerHost{manager: manager}
	request := tdriveplugin.WithHostCall(ctx)
	_, err = host.dispatch(request, "files.abortUpload", []byte(`{
		"jobId":"job-plugin-cancel","reason":"manual","state":"cancelled"
	}`))
	if err != nil {
		t.Fatalf("dispatch files.abortUpload: %v", err)
	}
	select {
	case <-workerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("plugin abort did not cancel the registered upload work")
	}
}

func TestUploadStreamCancellationUnblocksPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := newUploadStream(ctx)

	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(stream.reader(), make([]byte, 1))
		readDone <- err
	}()

	cancel()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pipe read error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled upload stream did not unblock its reader")
	}

	// The write side must be closed as well, or a plugin producer can remain
	// blocked even after the host has stopped consuming its bytes.
	if _, err := stream.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("pipe write error = %v, want io.ErrClosedPipe", err)
	}

	stream.finish(context.Canceled)
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream close error = %v, want context.Canceled", err)
	}
}
