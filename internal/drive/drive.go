package drive

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/tagcodec"
)

var (
	// ErrNoChannel means no storage channel has been chosen yet.
	ErrNoChannel = errors.New("drive: no storage channel is configured")
	// ErrExists is returned when a name is already taken in a directory.
	ErrExists = errors.New("drive: a file or directory with that name already exists")
	// ErrNotFound is returned for a path that does not resolve.
	ErrNotFound = errors.New("drive: no such file or directory")
	// ErrIsDir and ErrNotDir distinguish the two shape mismatches WebDAV
	// clients care about.
	ErrIsDir  = errors.New("drive: path is a directory")
	ErrNotDir = errors.New("drive: path is not a directory")
	// ErrLoop rejects moving a directory inside itself.
	ErrLoop = errors.New("drive: cannot move a directory into its own subtree")
)

// Service is the drive. One instance serves every user, because a tdrive
// deployment is one Telegram account and therefore one file tree.
type Service struct {
	cfg *config.Config
	db  *database.DB
	tg  Backend
	log *zap.Logger

	refs *refCache

	// OnRemoteProgress and OnDownloadProgress are set by the API layer so a
	// detached server-side transfer can still push progress to the browser.
	// Both are optional: the drive works without anyone listening.
	OnRemoteProgress   RemoteProgress
	OnDownloadProgress DownloadProgress

	// mkdirMu serialises directory creation. Two clients racing to create the
	// same folder would otherwise both send a Telegram message and one would
	// lose the unique-index race, leaving an orphaned record in the channel.
	mkdirMu sync.Mutex

	// Task limiters count whole logical file transfers. The upload job leases
	// and download sessions below let concurrent requests belonging to one
	// transfer share a single slot.
	uploadLimiter   *taskLimiter
	downloadLimiter *taskLimiter
	uploadJobsMu    sync.Mutex
	uploadJobs      map[string]*uploadJobLease

	downloadSessionsMu sync.Mutex
	downloadSessions   map[string]*downloadSession

	// stageMu serialises the decide-then-insert of a staged download, so two
	// requests for the same file cannot both conclude they are the first one.
	stageMu       sync.Mutex
	stageRunMu    sync.Mutex
	stageRuns     map[string]struct{}
	stageCancelMu sync.Mutex
	stageCancels  map[string]context.CancelFunc
}

func New(cfg *config.Config, db *database.DB, backend Backend, log *zap.Logger) *Service {
	settings := cfg.RuntimeSettings()
	return &Service{
		cfg:              cfg,
		db:               db,
		tg:               backend,
		log:              log,
		refs:             newRefCache(cfg.Stream.LocationTTL),
		uploadLimiter:    newTaskLimiter(settings.UploadConcurrency),
		downloadLimiter:  newTaskLimiter(settings.DownloadConcurrency),
		uploadJobs:       make(map[string]*uploadJobLease),
		downloadSessions: make(map[string]*downloadSession),
		stageRuns:        make(map[string]struct{}),
		stageCancels:     make(map[string]context.CancelFunc),
	}
}

// Entry is one listing row. Dir and File are mutually exclusive.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	MIME  string `json:"mime,omitempty"`
	// ID is the dirs or files row id, used by the operations that take an id
	// rather than a path.
	ID string `json:"id"`
	// SegmentCount is shown in the details panel. The listing itself always
	// presents one entry per logical file regardless of this number.
	SegmentCount int    `json:"segmentCount,omitempty"`
	Status       string `json:"status,omitempty"`
	ModifiedAt   int64  `json:"modifiedAt"`
	CreatedAt    int64  `json:"createdAt"`
}

// channelFor resolves a stored channel id, falling back to the default channel
// for rows written before a channel was recorded.
func (s *Service) channelFor(ctx context.Context, id string) (database.Channel, error) {
	if id != "" {
		ch, err := s.db.ChannelByID(ctx, id)
		if err == nil {
			return ch, nil
		}
		if !errors.Is(err, database.ErrNotFound) {
			return database.Channel{}, err
		}
	}
	ch, err := s.db.DefaultChannel(ctx)
	if errors.Is(err, database.ErrNotFound) {
		return database.Channel{}, ErrNoChannel
	}
	return ch, err
}

// storageChannel is where new records go.
func (s *Service) storageChannel(ctx context.Context) (database.Channel, error) {
	ch, err := s.db.DefaultChannel(ctx)
	if errors.Is(err, database.ErrNotFound) {
		return database.Channel{}, ErrNoChannel
	}
	return ch, err
}

// ref converts a stored channel row into the reference the backend takes.
func ref(ch database.Channel) ChannelRef {
	return ChannelRef{TGID: ch.TGID, AccessHash: ch.AccessHash}
}

// ResolveDir walks a path to its directory row. The root has no row, so it
// comes back as the zero Dir with an empty ID.
func (s *Service) ResolveDir(ctx context.Context, p string) (database.Dir, error) {
	clean, err := CleanPath(p)
	if err != nil {
		return database.Dir{}, err
	}
	if clean == Root {
		return database.Dir{Path: Root}, nil
	}

	dir, err := s.db.DirByPath(ctx, clean)
	if errors.Is(err, database.ErrNotFound) {
		return database.Dir{}, fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	return dir, err
}

// Stat resolves a path to either a directory or a file.
func (s *Service) Stat(ctx context.Context, p string) (Entry, error) {
	clean, err := CleanPath(p)
	if err != nil {
		return Entry{}, err
	}
	if clean == Root {
		return Entry{Name: "", Path: Root, IsDir: true, ID: ""}, nil
	}

	if dir, err := s.db.DirByPath(ctx, clean); err == nil {
		return dirEntry(dir), nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return Entry{}, err
	}

	parentPath, name := Parent(clean)
	parent, err := s.ResolveDir(ctx, parentPath)
	if err != nil {
		return Entry{}, err
	}
	f, err := s.db.FileInDir(ctx, parent.ID, name)
	if errors.Is(err, database.ErrNotFound) {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	if err != nil {
		return Entry{}, err
	}
	if f.Status == database.StatusPending {
		// A half-uploaded file has no readable bytes; pretending it is
		// absent is better than handing a client a truncated stream.
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	return fileEntry(f, clean), nil
}

// List returns the contents of a directory, directories first.
func (s *Service) List(ctx context.Context, p string) ([]Entry, error) {
	dir, err := s.ResolveDir(ctx, p)
	if err != nil {
		return nil, err
	}

	dirs, err := s.db.ListDirs(ctx, dir.ID)
	if err != nil {
		return nil, err
	}
	files, err := s.db.ListFiles(ctx, dir.ID)
	if err != nil {
		return nil, err
	}

	base := dir.Path
	if base == "" {
		base = Root
	}

	out := make([]Entry, 0, len(dirs)+len(files))
	for _, d := range dirs {
		out = append(out, dirEntry(d))
	}
	for _, f := range files {
		out = append(out, fileEntry(f, Join(base, f.Name)))
	}
	return out, nil
}

// Mkdir creates a directory, and every missing directory above it, recording
// each one as a tagged message in the channel first.
//
// Telegram is written before SQLite on purpose. If the process dies between the
// two, the channel holds a directory record the index has not seen yet, which
// the indexer picks up on the next rebuild. The other order would leave the
// index pointing at a message that does not exist.
func (s *Service) Mkdir(ctx context.Context, p string) (database.Dir, error) {
	clean, err := CleanPath(p)
	if err != nil {
		return database.Dir{}, err
	}
	if clean == Root {
		return database.Dir{Path: Root}, nil
	}

	s.mkdirMu.Lock()
	defer s.mkdirMu.Unlock()

	if existing, err := s.db.DirByPath(ctx, clean); err == nil {
		return existing, nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return database.Dir{}, err
	}

	parts := SplitPath(clean)
	var (
		parentID   string
		parentPath = Root
		current    database.Dir
	)
	for _, name := range parts {
		childPath := Join(parentPath, name)

		existing, err := s.db.DirByPath(ctx, childPath)
		switch {
		case err == nil:
			current, parentID, parentPath = existing, existing.ID, childPath
			continue
		case !errors.Is(err, database.ErrNotFound):
			return database.Dir{}, err
		}

		// A file already occupying the name blocks the directory.
		if _, err := s.db.FileInDir(ctx, parentID, name); err == nil {
			return database.Dir{}, fmt.Errorf("%w: %s", ErrExists, childPath)
		} else if !errors.Is(err, database.ErrNotFound) {
			return database.Dir{}, err
		}

		created, err := s.createDir(ctx, parentID, name, childPath)
		if err != nil {
			return database.Dir{}, err
		}
		current, parentID, parentPath = created, created.ID, childPath
	}
	return current, nil
}

func (s *Service) createDir(ctx context.Context, parentID, name, path string) (database.Dir, error) {
	channel, err := s.storageChannel(ctx)
	if err != nil {
		return database.Dir{}, err
	}
	id := database.NewID()
	caption, err := tagcodec.EncodeDir(id, parentID, name, path)
	if err != nil {
		return database.Dir{}, err
	}

	msgID, err := s.tg.SendRecord(ctx, ref(channel), caption)
	if err != nil {
		return database.Dir{}, fmt.Errorf("record directory %q in telegram: %w", path, err)
	}

	dir := database.Dir{
		ID:        id,
		ParentID:  parentID,
		Name:      name,
		Path:      path,
		ChannelID: channel.ID,
		TGMsgID:   msgID,
	}
	if err := s.db.InsertDir(ctx, dir); err != nil {
		// The message is already in the channel. Rather than leaving a
		// record the index will never match, take it back out.
		if delErr := s.tg.DeleteRecords(ctx, ref(channel), []int{msgID}); delErr != nil {
			s.log.Warn("could not roll back a directory record",
				zap.String("path", path), zap.Int("message", msgID), zap.Error(delErr))
		}
		if errors.Is(err, database.ErrConflict) {
			return database.Dir{}, fmt.Errorf("%w: %s", ErrExists, path)
		}
		return database.Dir{}, err
	}
	return dir, nil
}

func dirEntry(d database.Dir) Entry {
	return Entry{
		Name:       d.Name,
		Path:       d.Path,
		IsDir:      true,
		ID:         d.ID,
		ModifiedAt: d.UpdatedAt.UnixMilli(),
		CreatedAt:  d.CreatedAt.UnixMilli(),
	}
}

func fileEntry(f database.File, path string) Entry {
	return Entry{
		Name:         f.Name,
		Path:         path,
		IsDir:        false,
		Size:         f.Size,
		MIME:         f.MIME,
		ID:           f.ID,
		SegmentCount: f.SegmentCount,
		Status:       string(f.Status),
		ModifiedAt:   f.UpdatedAt.UnixMilli(),
		CreatedAt:    f.CreatedAt.UnixMilli(),
	}
}

// GuessMIME picks a content type from a filename. Telegram stores whatever we
// declare, and the browser player depends on getting video/* back.
func GuessMIME(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv":
		return "video/x-matroska"
	case ".ts":
		return "video/mp2t"
	case ".m4v":
		return "video/x-m4v"
	case ".flac":
		return "audio/flac"
	case ".7z":
		return "application/x-7z-compressed"
	case ".rar":
		return "application/vnd.rar"
	}
	return "application/octet-stream"
}
