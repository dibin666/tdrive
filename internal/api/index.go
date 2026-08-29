package api

import (
	"net/http"
	"sync"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
	"github.com/dibin/tdrive/internal/indexer"
)

// handleRebuildIndex discards the SQLite index and reconstructs it from the
// channel's captions.
//
// This is the disaster-recovery path, and it doubles as the proof that the
// caption format carries everything: if a rebuild does not reproduce the drive,
// the writer is not recording enough.
func (s *Server) handleRebuildIndex(w http.ResponseWriter, r *http.Request) {
	if err := s.index.Start(r.Context()); err != nil {
		s.fail(w, err, "rebuild index")
		return
	}
	writeJSON(w, http.StatusAccepted, s.index.Status())
}

func (s *Server) handleIndexStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.index.Status())
}

// WireIndexProgress forwards rebuild progress to connected browsers. It is set
// up at startup rather than per-request, because a rebuild outlives the request
// that started it.
func WireIndexProgress(idx *indexer.Indexer, broker *events.Broker) {
	idx.OnProgress = func(p indexer.Progress) {
		broker.Publish(events.Event{
			Type: events.TypeIndex,
			Data: events.IndexProgress{
				Scanned:  p.Scanned,
				Dirs:     p.Dirs,
				Files:    p.Files,
				Segments: p.Segments,
				Done:     p.Done,
				Error:    p.Error,
			},
		})
		if p.Done {
			// The whole tree just changed underneath every open browser.
			broker.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: "/"}})
		}
	}
}

// WireRemoteProgress forwards server-side fetch progress. Those transfers run
// detached from any request, so the only way a browser learns about them is
// through the event stream.
func WireRemoteProgress(svc *drive.Service, broker *events.Broker) {
	var throttles sync.Map
	svc.OnRemoteProgress = func(job database.UploadJob, uploaded, total int64, err error) {
		status := database.JobRunning
		message := ""
		switch {
		case err != nil:
			status, message = database.JobFailed, err.Error()
		case job.Status == database.JobComplete || total > 0 && uploaded >= total:
			status = database.JobComplete
		}
		// Terminal events always go out; intermediate ones are rate limited per job.
		if status == database.JobRunning {
			tVal, _ := throttles.LoadOrStore(job.ID, newProgressThrottle())
			throttle := tVal.(*progressThrottle)
			if !throttle.ready() {
				return
			}
		} else {
			throttles.Delete(job.ID)
		}
		broker.Publish(events.Event{
			Type:   events.TypeUpload,
			UserID: job.UserID,
			Data: events.UploadProgress{
				JobID:        job.ID,
				FileID:       job.FileID,
				Name:         job.Name,
				Uploaded:     uploaded,
				Total:        total,
				SegmentCount: job.SegmentCount,
				Status:       string(status),
				Error:        message,
				Source:       job.Source,
				SourceURL:    job.SourceURL,
			},
		})
		if status == database.JobComplete {
			// The destination path is not part of an upload progress event,
			// and resolving it here would add a database read to every final
			// callback. A root refresh is cheap and covers every server-side
			// source, including VPS-local uploads.
			broker.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: drive.Root}})
		}
	}
}
