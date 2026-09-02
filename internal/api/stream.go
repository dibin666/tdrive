package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
)

// requireFileAccess guards the byte-serving route. It accepts the usual
// credentials, and falls back to a media token scoped to the one file in the
// URL — which is the only way a <video> tag or a browser download can
// authenticate, since neither can set a request header.
func (s *Server) requireFileAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.URL.Query().Get("t"); token != "" {
			if s.auth.VerifyMediaToken(chi.URLParam(r, "id"), token) {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusUnauthorized, "this media link has expired")
			return
		}
		s.auth.RequireAuth(next).ServeHTTP(w, r)
	})
}

// handleMediaLink issues a short-lived URL for playback or download.
//
// This is the in-session link: it is bound to one file, expires in hours and
// exists because a <video> element cannot send an Authorization header. The
// durable, pasteable link an external downloader wants is a share link, minted
// through /files/{id}/share.
func (s *Server) handleMediaLink(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	file, err := s.fileForUser(r, id)
	if err != nil {
		s.fail(w, err, "media link")
		return
	}
	token := s.auth.SignMediaToken(file.ID)
	writeJSON(w, http.StatusOK, map[string]string{
		"url":      fmt.Sprintf("/api/files/%s/raw?t=%s", file.ID, url.QueryEscape(token)),
		"download": fmt.Sprintf("/api/files/%s/raw?download=1&t=%s", file.ID, url.QueryEscape(token)),
	})
}

// byteWindow is the slice of a logical file a request is about to serve.
//
// Whole-file and per-segment downloads differ only in this window, so they
// share every line of range parsing, header setting and copying below. That
// matters because the two have to agree exactly — a client downloading the
// segments separately and joining them must end up with the same bytes as one
// that took the whole file in a single request.
type byteWindow struct {
	file database.File
	// offset and length locate the window inside the logical file.
	offset int64
	length int64
	// name is what the browser should save the download as.
	name string
	// etag pins these particular bytes.
	etag string
	// sessionKey groups the parallel range requests of one logical download
	// so they share a single task slot.
	sessionKey string
	// attachment forces a download rather than letting the browser render the
	// bytes inline.
	attachment bool
}

func wholeFileWindow(file database.File, sessionKey string) byteWindow {
	return byteWindow{
		file:       file,
		offset:     0,
		length:     file.Size,
		name:       file.Name,
		etag:       fmt.Sprintf(`"%s-%d"`, file.ID, file.Size),
		sessionKey: sessionKey,
	}
}

// segmentWindow describes one stored segment as a standalone download.
func segmentWindow(file database.File, index int, sessionKey string) (byteWindow, error) {
	if index < 1 || index > file.SegmentCount {
		return byteWindow{}, fmt.Errorf("%w: segment %d of %d",
			database.ErrNotFound, index, file.SegmentCount)
	}
	length := drive.SegmentSize(file.Size, file.SegmentSize, index)
	return byteWindow{
		file:   file,
		offset: int64(index-1) * file.SegmentSize,
		length: length,
		// The .partNNN suffix matches the name the segment carries inside the
		// Telegram channel, so a person joining the pieces by hand sees the
		// same ordering in both places.
		name:       fmt.Sprintf("%s.part%03d", file.Name, index),
		etag:       fmt.Sprintf(`"%s-%d-%d"`, file.ID, index, length),
		sessionKey: sessionKey,
	}, nil
}

// handleRaw serves a whole file's bytes.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	file, err := s.db.FileByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "read file")
		return
	}
	// A media token authorises one file id and nothing else, so scope and
	// permission checks only apply to the credentialed path.
	if r.URL.Query().Get("t") == "" {
		if err := s.checkFileAccess(r, file); err != nil {
			s.fail(w, err, "read file")
			return
		}
	}
	s.serveBytes(w, r, wholeFileWindow(file, s.downloadSessionKey(r, file.ID, "")))
}

// handleSegmentRaw serves one stored segment as its own file.
//
// This is what makes the split-download mode possible: each segment is an
// independent, resumable, parallel-downloadable object, and the client joins
// them locally. For a many-segment file that is strictly more robust than one
// stream stitched across a dozen Telegram documents, because a failure costs
// one part rather than the whole transfer.
func (s *Server) handleSegmentRaw(w http.ResponseWriter, r *http.Request) {
	file, err := s.db.FileByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "read segment")
		return
	}
	if r.URL.Query().Get("t") == "" {
		if err := s.checkFileAccess(r, file); err != nil {
			s.fail(w, err, "read segment")
			return
		}
	}

	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "segment index must be a positive integer")
		return
	}
	window, err := segmentWindow(file, index, s.downloadSessionKey(r, file.ID, ""))
	if err != nil {
		s.fail(w, err, "read segment")
		return
	}
	s.serveBytes(w, r, window)
}

// serveBytes answers a byte request with full range support.
//
// Ranges are parsed here rather than delegated to http.ServeContent because
// the reader wants to know how much was asked for: ServeContent seeks and then
// copies a bounded amount, leaving the reader to guess how far to prefetch. A
// browser scrubbing a video sends a range per seek, and reading the range
// explicitly is what keeps each of those cheap.
func (s *Server) serveBytes(w http.ResponseWriter, r *http.Request, window byteWindow) {
	file := window.file
	if file.Status == database.StatusPending {
		writeError(w, http.StatusConflict, "this file is still uploading")
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", file.MIME)
	w.Header().Set("Last-Modified", file.UpdatedAt.UTC().Format(http.TimeFormat))
	// The id and size together pin a specific stored file; a rename does not
	// change the bytes, so it must not change the tag.
	w.Header().Set("ETag", window.etag)
	setDisposition(w, r, window.name, window.attachment)

	start, end, ranged, ok := parseRange(r.Header.Get("Range"), window.length)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", window.length))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "the requested range is not satisfiable")
		return
	}

	// A client that asked for a range gets 206 even when the range happens to
	// cover the whole file. "bytes=0-" is how a range-aware reader probes for
	// range support, and answering it with 200 tells that reader the server has
	// none — mediabunny then abandons seeking and streams a multi-gigabyte MKV
	// from byte zero just to reach the cues at its end, which never finishes.
	full := window.length == 0 || !ranged
	if full {
		w.Header().Set("Content-Length", strconv.FormatInt(window.length, 10))
	} else {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, window.length))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	}

	if r.Method == http.MethodHead {
		if !full {
			w.WriteHeader(http.StatusPartialContent)
		}
		return
	}
	if window.length == 0 {
		return
	}

	// Every range request belonging to one logical download shares a task
	// slot, so a parallel downloader counts as one transfer against the
	// configured limit rather than as one per connection. The lease is held
	// until this response's copy ends.
	// The slot carries the Telegram account the whole download runs on, so a
	// multi-connection client stays on one login rather than spending several
	// accounts' budgets on one file.
	account, release, err := s.drive.AcquireDownloadSession(r.Context(), window.sessionKey, file.ID)
	if err != nil {
		s.fail(w, err, "open file")
		return
	}
	defer release()

	// A staged copy on local disk is preferred over pulling the same bytes out
	// of Telegram again. It is checked after the lease so that a client cannot
	// dodge the queue by racing to a copy that is still being written.
	if path, ok := s.stagedCopy(r, file); ok {
		if s.serveStagedRange(w, r, path, window, start, end, full) {
			return
		}
	}

	if !full {
		w.WriteHeader(http.StatusPartialContent)
	}

	reader, err := s.drive.OpenFile(r.Context(), file, account)
	if err != nil {
		s.fail(w, err, "open file")
		return
	}
	defer reader.Close()

	if seekTo := window.offset + start; seekTo > 0 {
		if _, err := reader.Seek(seekTo, io.SeekStart); err != nil {
			s.fail(w, err, "seek file")
			return
		}
	}

	if copied, err := io.CopyN(w, reader, end-start+1); err != nil {
		s.drive.RecordDownloadSessionBytes(window.sessionKey, copied)
		// A player seeking away or a client disconnecting mid-stream is
		// routine, not an error worth alarming about. The headers are
		// already sent, so there is nothing to report to the client anyway.
		if !isClientGone(err) {
			s.log.Warn("stream ended early",
				zap.String("file", file.ID), zap.Error(err))
		}
	} else {
		s.drive.RecordDownloadSessionBytes(window.sessionKey, copied)
	}
}

// stagedCopy reports a ready staged copy of a file, if one exists. Serving
// from it is transparent to the client and is the fastest path available.
func (s *Server) stagedCopy(r *http.Request, file database.File) (string, bool) {
	job, err := s.db.StagedDownloadFor(r.Context(), file.ID, nowMillis())
	if err != nil || job.CachePath == "" {
		return "", false
	}
	if _, err := os.Stat(job.CachePath); err != nil {
		return "", false
	}
	_ = s.db.TouchDownload(r.Context(), job.ID)
	return job.CachePath, true
}

// serveStagedRange copies the requested range out of a local staged file. It
// reports false if the copy turns out to be unusable, so the caller can fall
// back to Telegram without having written anything to the response.
func (s *Server) serveStagedRange(
	w http.ResponseWriter,
	r *http.Request,
	path string,
	window byteWindow,
	start, end int64,
	full bool,
) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() < window.offset+end+1 {
		// A short staged file means the copy is stale or was truncated;
		// Telegram remains the source of truth.
		return false
	}
	if _, err := f.Seek(window.offset+start, io.SeekStart); err != nil {
		return false
	}

	if !full {
		w.WriteHeader(http.StatusPartialContent)
	}
	if _, err := io.CopyN(w, f, end-start+1); err != nil && !isClientGone(err) {
		s.log.Warn("staged stream ended early",
			zap.String("file", window.file.ID), zap.Error(err))
	}
	return true
}

// downloadSessionKey groups the requests that belong to one logical download.
//
// Getting this grouping right is what lets the whole-file concurrency limit
// and multi-connection downloading coexist: eight parallel ranges of one file
// must count as one transfer, while two different files must count as two.
// shareID takes precedence because a share link is a transfer in its own
// right and may not have a session behind it at all.
func (s *Server) downloadSessionKey(r *http.Request, fileID, shareID string) string {
	if shareID != "" {
		return "share:" + shareID
	}
	if user, ok := auth.FromContext(r.Context()); ok && user.ID != "" {
		return "user:" + user.ID + ":" + fileID
	}
	// A media token carries no account, so the client address is the only
	// grouping available. Two viewers behind one NAT sharing a slot is a far
	// better failure than one viewer's player consuming eight.
	return "media:" + fileID + ":" + auth.ClientFrom(r).IP
}

// setDisposition asks the browser to download rather than render.
//
// A share link defaults to attachment: it exists to be handed to a download
// manager or clicked on a phone, and having the browser try to render a 5 GB
// MKV instead of saving it is never what was wanted. The in-session media link
// defaults the other way, because that one feeds a <video> element.
func setDisposition(w http.ResponseWriter, r *http.Request, name string, attachment bool) {
	kind := "inline"
	if attachment || r.URL.Query().Get("download") == "1" {
		kind = "attachment"
	}
	if r.URL.Query().Get("inline") == "1" {
		kind = "inline"
	}
	// Both forms are emitted: filename for older clients, filename* with the
	// RFC 5987 encoding so non-ASCII names survive.
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q; filename*=UTF-8''%s",
		kind, sanitizeFilename(name), url.PathEscape(name)))
}

// sanitizeFilename strips what cannot appear in a quoted header value.
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == '"' || r == '\\' || r > 0x7e {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

// parseRange handles the single-range forms that matter: "bytes=a-b",
// "bytes=a-" and the suffix form "bytes=-n". Multipart ranges are deliberately
// not supported; no player or WebDAV client sends them, and answering with the
// whole file is a valid response to a range header a server cannot satisfy in
// parts.
//
// ranged reports whether the caller actually asked for a range, which decides
// between 200 and 206. It is separate from the bounds because a satisfied
// "bytes=0-" covers the whole file and still has to be answered as partial
// content.
func parseRange(header string, size int64) (start, end int64, ranged, ok bool) {
	if header == "" || size == 0 {
		return 0, max(size-1, 0), false, true
	}
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found {
		// An unknown range unit is ignored, which RFC 9110 allows and which is
		// not a range request as far as the response is concerned.
		return 0, size - 1, false, true
	}
	// Only the first range of a multi-range request is honoured.
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}

	before, after, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false, false
	}
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)

	switch {
	case before == "" && after == "":
		return 0, 0, false, false

	case before == "":
		// Suffix form: the last n bytes.
		n, err := strconv.ParseInt(after, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true

	default:
		start, err := strconv.ParseInt(before, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false, false
		}
		end = size - 1
		if after != "" {
			end, err = strconv.ParseInt(after, 10, 64)
			if err != nil || end < start {
				return 0, 0, false, false
			}
			if end > size-1 {
				end = size - 1
			}
		}
		return start, end, true, true
	}
}

// isClientGone recognises the errors that mean the other end hung up.
func isClientGone(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, http.ErrHandlerTimeout) ||
		errors.Is(err, io.EOF) ||
		strings.Contains(err.Error(), "broken pipe") ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "context canceled")
}
