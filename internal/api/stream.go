package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
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
func (s *Server) handleMediaLink(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	file, err := s.db.FileByID(r.Context(), id)
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

// handleRaw serves a file's bytes with full range support.
//
// Ranges are parsed here rather than delegated to http.ServeContent because the
// reader wants to know how much was asked for: ServeContent seeks and then
// copies a bounded amount, leaving the reader to guess how far to prefetch. A
// browser scrubbing a video sends a range per seek, and reading the range
// explicitly is what keeps each of those cheap.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	file, err := s.db.FileByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "read file")
		return
	}
	if file.Status == database.StatusPending {
		writeError(w, http.StatusConflict, "this file is still uploading")
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", file.MIME)
	w.Header().Set("Last-Modified", file.UpdatedAt.UTC().Format(http.TimeFormat))
	// The id and size together pin a specific stored file; a rename does not
	// change the bytes, so it must not change the tag.
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%d"`, file.ID, file.Size))
	setDisposition(w, r, file.Name)

	start, end, ok := parseRange(r.Header.Get("Range"), file.Size)
	switch {
	case !ok:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", file.Size))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "the requested range is not satisfiable")
		return
	case start == 0 && end == file.Size-1:
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	default:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	}

	if r.Method == http.MethodHead {
		return
	}
	if file.Size == 0 {
		return
	}

	reader, err := s.drive.OpenFile(r.Context(), file)
	if err != nil {
		s.fail(w, err, "open file")
		return
	}
	defer reader.Close()

	if start > 0 {
		if _, err := reader.Seek(start, io.SeekStart); err != nil {
			s.fail(w, err, "seek file")
			return
		}
	}

	if _, err := io.CopyN(w, reader, end-start+1); err != nil {
		// A player seeking away or a client disconnecting mid-stream is
		// routine, not an error worth alarming about. The headers are
		// already sent, so there is nothing to report to the client anyway.
		if !isClientGone(err) {
			s.log.Warn("stream ended early",
				zap.String("file", file.ID), zap.Error(err))
		}
	}
}

// setDisposition asks the browser to download rather than render, unless the
// caller explicitly wants inline playback.
func setDisposition(w http.ResponseWriter, r *http.Request, name string) {
	kind := "inline"
	if r.URL.Query().Get("download") == "1" {
		kind = "attachment"
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
func parseRange(header string, size int64) (start, end int64, ok bool) {
	if header == "" || size == 0 {
		return 0, max(size-1, 0), true
	}
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found {
		return 0, size - 1, true
	}
	// Only the first range of a multi-range request is honoured.
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}

	before, after, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false
	}
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)

	switch {
	case before == "" && after == "":
		return 0, 0, false

	case before == "":
		// Suffix form: the last n bytes.
		n, err := strconv.ParseInt(after, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true

	default:
		start, err := strconv.ParseInt(before, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false
		}
		end = size - 1
		if after != "" {
			end, err = strconv.ParseInt(after, 10, 64)
			if err != nil || end < start {
				return 0, 0, false
			}
			if end > size-1 {
				end = size - 1
			}
		}
		return start, end, true
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
