package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
)

// Access checks live here rather than being scattered through the handlers,
// because two of them — the permission bit and the directory scope — have to
// be applied at every entry point that reaches the drive, and a handler that
// forgets one is not a bug anybody notices until it matters.

func nowMillis() int64 { return time.Now().UnixMilli() }

// scopeOf returns the requesting account's view of the drive.
func (s *Server) scopeOf(r *http.Request) drive.Scope {
	user, ok := auth.FromContext(r.Context())
	if !ok {
		return drive.Scope{}
	}
	return drive.ScopeOf(user)
}

// realPath converts a client-supplied path into a real drive path, rejecting
// anything outside the account's scope.
func (s *Server) realPath(r *http.Request, p string) (string, error) {
	if p == "" {
		p = drive.Root
	}
	return s.scopeOf(r).ToReal(p)
}

// visiblePath converts a real drive path back into the client's view. It
// returns ErrOutOfScope for a path the account should not be shown, which the
// error mapping renders as a 404.
func (s *Server) visiblePath(r *http.Request, real string) (string, error) {
	visible, ok := s.scopeOf(r).ToVisible(real)
	if !ok {
		return "", fmt.Errorf("%w: %s", drive.ErrOutOfScope, real)
	}
	return visible, nil
}

// requirePerm is the handler-level form of the permission check, for the
// endpoints that need it conditionally rather than for a whole route group.
func requirePerm(r *http.Request, perm database.Perm) error {
	user, ok := auth.FromContext(r.Context())
	if !ok {
		return auth.ErrBadCredentials
	}
	if !user.Can(perm) {
		return auth.ErrForbidden
	}
	return nil
}

// filePath resolves a stored file to its full drive path.
func (s *Server) filePath(ctx context.Context, file database.File) (string, error) {
	if file.DirID == "" {
		return drive.Join(drive.Root, file.Name), nil
	}
	dir, err := s.db.DirByID(ctx, file.DirID)
	if err != nil {
		return "", err
	}
	return drive.Join(dir.Path, file.Name), nil
}

// checkFileAccess verifies that the requesting account may read a file's
// bytes: it needs the download permission, and the file has to lie inside its
// scope.
func (s *Server) checkFileAccess(r *http.Request, file database.File) error {
	if err := requirePerm(r, database.PermDownload); err != nil {
		return err
	}
	scope := s.scopeOf(r)
	if scope.Unrestricted() {
		return nil
	}
	path, err := s.filePath(r.Context(), file)
	if err != nil {
		return err
	}
	if !scope.Contains(path) {
		return fmt.Errorf("%w: %s", drive.ErrOutOfScope, file.Name)
	}
	return nil
}

// fileForUser loads a file and applies the same access check, so a handler
// that only has an id does not have to remember both steps.
func (s *Server) fileForUser(r *http.Request, id string) (database.File, error) {
	file, err := s.db.FileByID(r.Context(), id)
	if err != nil {
		return database.File{}, err
	}
	if err := s.checkFileAccess(r, file); err != nil {
		return database.File{}, err
	}
	return file, nil
}

// externalOrigin is the scheme and host a client should use to reach this
// server, which is what a pasteable download link has to be built from.
//
// The configured base URL wins because it is the only value that is correct
// behind a reverse proxy that rewrites the host. Falling back to the request
// mirrors what secureCookies already does for the same reason.
func (s *Server) externalOrigin(r *http.Request) string {
	if s.cfg.Server.BaseURL != "" {
		return strings.TrimSuffix(s.cfg.Server.BaseURL, "/")
	}

	scheme := "http"
	if s.secureCookies(r) {
		scheme = "https"
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		// Only the first entry is the original client-facing host; the rest
		// are the proxies it passed through.
		if i := strings.IndexByte(forwarded, ','); i >= 0 {
			forwarded = forwarded[:i]
		}
		host = strings.TrimSpace(forwarded)
	}
	return scheme + "://" + host
}
