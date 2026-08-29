// Package dav exposes the drive over WebDAV.
//
// The whole point of this package is that segments are invisible here. A file
// stored as seven Telegram documents appears in a PROPFIND as one resource of
// one size, and a client reading it — rclone, Finder, Windows Explorer — gets
// one continuous stream. Everything that makes that work already lives in
// internal/reader and internal/drive; this package is the adapter.
package dav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/webdav"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
)

// FileSystem adapts the drive to webdav.FileSystem.
type FileSystem struct {
	cfg   *config.Config
	db    *database.DB
	drive *drive.Service
	log   *zap.Logger
}

// Handler builds the mountable WebDAV handler, guarded by Basic auth.
func Handler(
	cfg *config.Config,
	db *database.DB,
	driveSvc *drive.Service,
	authSvc *auth.Service,
	log *zap.Logger,
) http.Handler {
	dav := &webdav.Handler{
		Prefix:     cfg.WebDAV.Prefix,
		FileSystem: &FileSystem{cfg: cfg, db: db, drive: driveSvc, log: log},
		// An in-memory lock system is the right scope here: locks are
		// advisory, and a restart that drops them is less harmful than a
		// stale lock nobody can clear.
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Debug("webdav request failed",
					zap.String("method", r.Method),
					zap.String("path", r.URL.Path),
					zap.Error(err))
			}
		},
	}
	return authSvc.RequireBasic(withContentLength(dav))
}

// contentLengthKey carries a PUT's declared body length into OpenFile.
//
// webdav.FileSystem.OpenFile receives only a context, but the length decides
// whether an upload can stream straight into Telegram or has to be spooled to
// disk first, so it is threaded through here rather than guessed.
type contentLengthKey struct{}

func withContentLength(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.ContentLength >= 0 {
			ctx := context.WithValue(r.Context(), contentLengthKey{}, r.ContentLength)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func contentLengthFrom(ctx context.Context) (int64, bool) {
	n, ok := ctx.Value(contentLengthKey{}).(int64)
	return n, ok
}

func (f *FileSystem) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	clean, err := drive.CleanPath(name)
	if err != nil {
		return translate(err)
	}
	// MKCOL must fail if the collection already exists, and must not create
	// intermediate directories — RFC 4918 is explicit about both.
	if _, err := f.drive.Stat(ctx, clean); err == nil {
		return os.ErrExist
	}
	parent, _ := drive.Parent(clean)
	if parent != drive.Root {
		if _, err := f.drive.ResolveDir(ctx, parent); err != nil {
			return translate(err)
		}
	}
	_, err = f.drive.Mkdir(ctx, clean)
	return translate(err)
}

func (f *FileSystem) RemoveAll(ctx context.Context, name string) error {
	clean, err := drive.CleanPath(name)
	if err != nil {
		return translate(err)
	}
	if clean == drive.Root {
		return os.ErrPermission
	}
	return translate(f.drive.Delete(ctx, clean))
}

func (f *FileSystem) Rename(ctx context.Context, oldName, newName string) error {
	from, err := drive.CleanPath(oldName)
	if err != nil {
		return translate(err)
	}
	to, err := drive.CleanPath(newName)
	if err != nil {
		return translate(err)
	}
	if from == drive.Root || to == drive.Root {
		return os.ErrPermission
	}

	fromDir, fromBase := drive.Parent(from)
	toDir, toBase := drive.Parent(to)

	// A MOVE can change the directory, the name, or both, and the drive
	// exposes those as separate operations.
	if fromDir != toDir {
		if _, err := f.drive.Move(ctx, from, toDir); err != nil {
			return translate(err)
		}
		from = drive.Join(toDir, fromBase)
	}
	if fromBase != toBase {
		if _, err := f.drive.Rename(ctx, from, toBase); err != nil {
			return translate(err)
		}
	}
	return nil
}

func (f *FileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	clean, err := drive.CleanPath(name)
	if err != nil {
		return nil, translate(err)
	}
	entry, err := f.drive.Stat(ctx, clean)
	if err != nil {
		return nil, translate(err)
	}
	return &fileInfo{entry: entry}, nil
}

// OpenFile serves both reads and writes. The flags tell them apart: WebDAV PUT
// arrives with O_WRONLY|O_CREATE|O_TRUNC.
func (f *FileSystem) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	clean, err := drive.CleanPath(name)
	if err != nil {
		return nil, translate(err)
	}

	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		return f.openForWrite(ctx, clean, flag)
	}

	entry, err := f.drive.Stat(ctx, clean)
	if err != nil {
		return nil, translate(err)
	}
	if entry.IsDir {
		return &dirHandle{fs: f, ctx: ctx, path: clean, info: fileInfo{entry: entry}}, nil
	}

	file, err := f.db.FileByID(ctx, entry.ID)
	if err != nil {
		return nil, translate(err)
	}
	return &readHandle{fs: f, ctx: ctx, file: file, info: fileInfo{entry: entry}}, nil
}

func (f *FileSystem) openForWrite(ctx context.Context, clean string, flag int) (webdav.File, error) {
	if clean == drive.Root {
		return nil, os.ErrPermission
	}
	dir, name := drive.Parent(clean)

	// O_EXCL means "must not exist", which is how some clients express a
	// create-only PUT.
	if flag&os.O_EXCL != 0 {
		if _, err := f.drive.Stat(ctx, clean); err == nil {
			return nil, os.ErrExist
		}
	}
	if _, err := f.drive.ResolveDir(ctx, dir); err != nil {
		return nil, translate(err)
	}

	user, _ := auth.FromContext(ctx)
	return newWriteHandle(ctx, f, dir, name, user.ID), nil
}

// fileInfo adapts a drive entry to os.FileInfo. Size is the logical size of the
// whole file, which is the number every WebDAV client is shown.
type fileInfo struct {
	entry drive.Entry
}

func (i *fileInfo) Name() string { return i.entry.Name }
func (i *fileInfo) Size() int64  { return i.entry.Size }
func (i *fileInfo) Mode() fs.FileMode {
	if i.entry.IsDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i *fileInfo) ModTime() time.Time { return time.UnixMilli(i.entry.ModifiedAt) }
func (i *fileInfo) IsDir() bool        { return i.entry.IsDir }
func (i *fileInfo) Sys() any           { return nil }

// ContentType lets webdav report the stored MIME type in PROPFIND instead of
// guessing from the extension.
func (i *fileInfo) ContentType(context.Context) (string, error) {
	if i.entry.IsDir {
		return "httpd/unix-directory", nil
	}
	if i.entry.MIME != "" {
		return i.entry.MIME, nil
	}
	return "application/octet-stream", nil
}

// ETag pins the stored bytes. It deliberately excludes the name, so a rename
// does not invalidate a client's cache of content that has not changed.
func (i *fileInfo) ETag(context.Context) (string, error) {
	if i.entry.IsDir {
		return fmt.Sprintf(`"%s-d%d"`, i.entry.ID, i.entry.ModifiedAt), nil
	}
	return fmt.Sprintf(`"%s-%d"`, i.entry.ID, i.entry.Size), nil
}

// dirHandle answers Readdir for PROPFIND.
type dirHandle struct {
	fs   *FileSystem
	ctx  context.Context
	path string
	info fileInfo

	once    sync.Once
	entries []os.FileInfo
	err     error
	offset  int
}

func (d *dirHandle) load() {
	d.once.Do(func() {
		entries, err := d.fs.drive.List(d.ctx, d.path)
		if err != nil {
			d.err = translate(err)
			return
		}
		d.entries = make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			// A file whose segments are incomplete has no coherent byte
			// stream. Listing it would invite a client to copy a corrupt
			// file; the WebUI shows it with a warning instead.
			if e.Status == string(database.StatusBroken) {
				continue
			}
			d.entries = append(d.entries, &fileInfo{entry: e})
		}
	})
}

func (d *dirHandle) Readdir(count int) ([]os.FileInfo, error) {
	d.load()
	if d.err != nil {
		return nil, d.err
	}
	if count <= 0 {
		rest := d.entries[d.offset:]
		d.offset = len(d.entries)
		return rest, nil
	}
	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}
	end := min(d.offset+count, len(d.entries))
	page := d.entries[d.offset:end]
	d.offset = end
	return page, nil
}

func (d *dirHandle) Stat() (os.FileInfo, error) { return &d.info, nil }
func (d *dirHandle) Close() error               { return nil }
func (d *dirHandle) Read([]byte) (int, error)   { return 0, os.ErrInvalid }
func (d *dirHandle) Write([]byte) (int, error)  { return 0, os.ErrPermission }
func (d *dirHandle) Seek(int64, int) (int64, error) {
	return 0, os.ErrInvalid
}

// translate maps drive errors onto the os errors webdav.Handler understands, so
// clients receive 404, 409 and 403 rather than a blanket 500.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, drive.ErrNotFound), errors.Is(err, database.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, drive.ErrExists), errors.Is(err, database.ErrConflict):
		return os.ErrExist
	case errors.Is(err, drive.ErrLoop):
		return os.ErrInvalid
	}

	var pathErr *drive.PathError
	if errors.As(err, &pathErr) {
		return os.ErrInvalid
	}
	return err
}
