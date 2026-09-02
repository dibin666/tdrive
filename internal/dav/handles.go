package dav

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/reader"
)

// readHandle serves GET. The reader underneath stitches segments together, so
// nothing here is aware that a file might be split.
//
// The stream and its download-task lease are opened lazily. webdav.Handler calls
// http.ServeContent, which seeks to the end and back purely to measure the file,
// and a PROPFIND-heavy client would otherwise open a Telegram connection for
// every entry it looks at.
type readHandle struct {
	fs     *FileSystem
	ctx    context.Context
	file   database.File
	info   fileInfo
	userID string

	// sessionKey groups this handle's reads with the other requests belonging
	// to the same logical download.
	sessionKey string

	mu      sync.Mutex
	stream  *reader.File
	release func()
	// track is the transfer-panel record for this download. It is shared with
	// every other request in the same session and settled once they have all
	// finished.
	track *drive.ClientRead
	pos   int64
}

func (h *readHandle) ensure() error {
	if h.stream != nil {
		return nil
	}
	// Every range request of one mounted read shares a session, and therefore
	// one Telegram account: the account is part of the slot, not chosen per
	// request. The primary is selected first and a fallback can take over when
	// it is unavailable.
	account, release, err := h.fs.drive.AcquireDownloadSession(h.ctx, h.sessionKey, h.file.ID)
	if err != nil {
		return translate(err)
	}
	stream, err := h.fs.drive.OpenFile(h.ctx, h.file, account)
	if err != nil {
		release()
		return translate(err)
	}
	if h.pos != 0 {
		if _, err := stream.Seek(h.pos, io.SeekStart); err != nil {
			stream.Close()
			release()
			return err
		}
	}

	// Tracking starts here rather than in OpenFile because webdav.Handler opens
	// and stats a file for reasons that never read a byte — PROPFIND, and
	// ServeContent measuring the length — and a transfer row for each of those
	// would be noise, not history. A failure to record is not a reason to
	// refuse the download.
	track, err := h.fs.drive.TrackClientDownload(
		h.ctx, h.sessionKey, h.userID, h.file, database.DownloadWebDAV)
	if err != nil {
		h.fs.log.Warn("could not record a webdav download",
			zap.String("file", h.file.ID), zap.Error(err))
	}

	h.stream = stream
	h.release = release
	h.track = track
	return nil
}

func (h *readHandle) Read(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.file.Size == 0 {
		return 0, io.EOF
	}
	if err := h.ensure(); err != nil {
		return 0, err
	}
	n, err := h.stream.Read(p)
	h.pos += int64(n)
	// A cancellation from the transfer panel arrives as an error here, because
	// refusing to serve more bytes is the only way to stop a mounted client.
	if trackErr := h.track.Add(int64(n)); trackErr != nil && err == nil {
		return n, trackErr
	}
	return n, err
}

func (h *readHandle) Seek(offset int64, whence int) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = h.pos + offset
	case io.SeekEnd:
		abs = h.file.Size + offset
	default:
		return 0, os.ErrInvalid
	}
	if abs < 0 {
		return 0, os.ErrInvalid
	}

	if h.stream != nil {
		if _, err := h.stream.Seek(abs, io.SeekStart); err != nil {
			return 0, err
		}
	}
	h.pos = abs
	return abs, nil
}

func (h *readHandle) Close() error {
	h.mu.Lock()
	var (
		release func()
		track   *drive.ClientRead
	)
	if h.stream != nil {
		err := h.stream.Close()
		h.stream = nil
		release, track = h.release, h.track
		h.release, h.track = nil, nil
		h.mu.Unlock()
		if release != nil {
			release()
		}
		track.Close()
		return err
	}
	h.mu.Unlock()
	return nil
}

func (h *readHandle) Stat() (os.FileInfo, error)         { return &h.info, nil }
func (h *readHandle) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (h *readHandle) Write([]byte) (int, error)          { return 0, os.ErrPermission }

// writeHandle receives a PUT.
//
// WebDAV gives the body to Write in pieces and only signals completion with
// Close, while Telegram needs the total length before the first byte goes out.
// A pipe bridges the two: the drive's segmented upload runs on its own
// goroutine, pulling from the pipe as fast as the client fills it, so a
// multi-gigabyte PUT never lands on disk. The drive acquires the global upload
// task lease before it starts consuming the pipe, so queued WebDAV PUTs apply
// backpressure to the client.
//
// When the client did declare a Content-Length, that length is used directly
// and the split happens on exact boundaries. Without one there is no honest
// alternative to measuring first, so those uploads spool — which is why
// Content-Length matters and why the drive advertises it.
type writeHandle struct {
	fs      *FileSystem
	ctx     context.Context
	dir     string
	name    string
	userID  string
	size    int64
	hasSize bool

	once    sync.Once
	writer  *io.PipeWriter
	done    chan error
	written int64

	mu     sync.Mutex
	closed bool
	result database.File
}

func newWriteHandle(ctx context.Context, fs *FileSystem, dir, name, userID string) *writeHandle {
	h := &writeHandle{fs: fs, ctx: ctx, dir: dir, name: name, userID: userID}
	if size, ok := contentLengthFrom(ctx); ok {
		h.size, h.hasSize = size, true
	}
	return h
}

func (h *writeHandle) start() {
	pr, pw := io.Pipe()
	h.writer = pw
	h.done = make(chan error, 1)

	req := drive.UploadRequest{
		DirPath:   h.dir,
		Name:      h.name,
		Size:      h.size,
		MIME:      drive.GuessMIME(h.name),
		UserID:    h.userID,
		Source:    "webdav",
		Overwrite: true, // PUT replaces, per RFC 4918
	}

	go func() {
		// The upload must outlive the request context: webdav closes the
		// handle after the body is read, and cancelling here would abort a
		// transfer that is otherwise complete.
		uploadCtx := context.WithoutCancel(h.ctx)

		var (
			file database.File
			err  error
		)
		if h.hasSize {
			file, err = h.fs.drive.UploadStream(uploadCtx, req, pr, nil)
		} else {
			file, err = h.fs.drive.UploadUnsized(uploadCtx, req, pr, nil)
		}

		if err != nil {
			// Unblocking the writer matters: without it a client whose
			// upload failed server-side would hang until it timed out.
			pr.CloseWithError(err)
		} else {
			pr.Close()
		}

		h.mu.Lock()
		h.result = file
		h.mu.Unlock()
		h.done <- err
	}()
}

func (h *writeHandle) Write(p []byte) (int, error) {
	h.once.Do(h.start)
	n, err := h.writer.Write(p)
	h.written += int64(n)

	if h.hasSize && h.written > h.size {
		// More bytes than declared would make Telegram's part accounting
		// wrong, so refuse rather than store something truncated.
		err = fmt.Errorf("body is longer than the declared Content-Length of %d", h.size)
		h.writer.CloseWithError(err)
		return n, err
	}
	return n, err
}

func (h *writeHandle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()

	// A PUT with no body at all still creates an empty file, so the upload
	// has to be started even if Write was never called.
	h.once.Do(h.start)

	if h.hasSize && h.written < h.size {
		err := fmt.Errorf("body ended after %d of the declared %d bytes", h.written, h.size)
		h.writer.CloseWithError(err)
		<-h.done
		return err
	}

	if err := h.writer.Close(); err != nil {
		return err
	}

	err := <-h.done
	if err != nil {
		h.fs.log.Warn("a webdav upload failed",
			zap.String("name", h.name), zap.String("dir", h.dir), zap.Error(err))
		return translate(err)
	}
	return nil
}

func (h *writeHandle) Stat() (os.FileInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Before the upload finishes there is no stored file to describe, so the
	// declared length is the best answer available.
	size := h.size
	if h.result.ID != "" {
		size = h.result.Size
	}
	return &fileInfo{entry: drive.Entry{
		Name: h.name,
		Path: drive.Join(h.dir, h.name),
		Size: size,
		MIME: drive.GuessMIME(h.name),
	}}, nil
}

func (h *writeHandle) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (h *writeHandle) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }

// Seek on a write handle is only tolerated for the no-op forms. Some clients
// probe with Seek(0, Current) before writing; a real reposition cannot be
// honoured by a streaming upload.
func (h *writeHandle) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && (whence == io.SeekStart || whence == io.SeekCurrent) && h.written == 0 {
		return 0, nil
	}
	return 0, os.ErrInvalid
}
