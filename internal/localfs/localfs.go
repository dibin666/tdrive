// Package localfs exposes a carefully scoped, read-only view of one directory
// on the server. It is used by the WebUI to choose files that already exist on
// the VPS, so those bytes can go straight from the container to Telegram.
package localfs

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrDisabled means the deployment did not configure a local source.
	ErrDisabled = errors.New("local file source is not configured")
	// ErrUnavailable means the configured bind mount is not currently present
	// or cannot be read.
	ErrUnavailable = errors.New("local file directory is unavailable")
	// ErrInvalidPath is returned for a path that attempts to leave the source
	// root or contains a path separator that cannot be represented safely.
	ErrInvalidPath = errors.New("invalid local file path")
	// ErrNotFound deliberately hides whether a requested path is absent or a
	// symlink was rejected.
	ErrNotFound = errors.New("local file was not found")
	// ErrNotFile and ErrNotDir let the API report a useful client error when the
	// caller selects the wrong kind of filesystem object.
	ErrNotFile = errors.New("local path is not a file")
	ErrNotDir  = errors.New("local path is not a directory")
)

// Entry is one item in a local directory listing. Paths are always relative
// to Root and use slash separators, including on platforms where the native
// separator differs.
type Entry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	IsDir      bool   `json:"isDir"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modifiedAt"`
}

// Service reads from one configured root. It never writes to that directory.
type Service struct {
	root string
}

// New constructs a local source. An empty root leaves it disabled; the mount
// may be created after the process starts, so existence is checked per call.
func New(root string) *Service {
	return &Service{root: strings.TrimSpace(root)}
}

// Enabled reports whether the deployment opted into the local source.
func (s *Service) Enabled() bool { return s != nil && s.root != "" }

// List returns the immediate children of a directory, with directories first.
// Symlinks and non-regular filesystem objects are omitted so a mounted source
// cannot be used to walk into another part of the host filesystem.
func (s *Service) List(requestPath string) ([]Entry, string, error) {
	clean, absolute, err := s.resolve(requestPath)
	if err != nil {
		return nil, "", err
	}

	info, err := os.Stat(absolute)
	if err != nil {
		return nil, "", unavailableOrNotFound(err)
	}
	if !info.IsDir() {
		return nil, "", ErrNotDir
	}

	items, err := os.ReadDir(absolute)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		// Lstat is intentional: even a symlink that points back inside the
		// root is not part of the exposed tree. This keeps the UI tree stable
		// and avoids a time-of-check/time-of-use escape through a link.
		if item.Type()&os.ModeSymlink != 0 {
			continue
		}
		itemInfo, err := item.Info()
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if !itemInfo.IsDir() && !itemInfo.Mode().IsRegular() {
			continue
		}

		entries = append(entries, Entry{
			Name:       item.Name(),
			Path:       childPath(clean, item.Name()),
			IsDir:      itemInfo.IsDir(),
			Size:       sizeOf(itemInfo),
			ModifiedAt: itemInfo.ModTime().UnixMilli(),
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, clean, nil
}

// Stat returns a single existing item. The caller can use it to validate a
// selection before starting a detached upload.
func (s *Service) Stat(requestPath string) (Entry, error) {
	clean, absolute, err := s.resolve(requestPath)
	if err != nil {
		return Entry{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Entry{}, unavailableOrNotFound(err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return Entry{}, ErrNotFound
	}
	return Entry{
		Name:       info.Name(),
		Path:       clean,
		IsDir:      info.IsDir(),
		Size:       sizeOf(info),
		ModifiedAt: info.ModTime().UnixMilli(),
	}, nil
}

// Open opens one regular file under Root. The returned file is read-only and
// the caller owns its Close method.
func (s *Service) Open(requestPath string) (*os.File, Entry, error) {
	entry, err := s.Stat(requestPath)
	if err != nil {
		return nil, Entry{}, err
	}
	if entry.IsDir {
		return nil, Entry{}, ErrNotFile
	}

	_, absolute, err := s.resolve(requestPath)
	if err != nil {
		return nil, Entry{}, err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, Entry{}, unavailableOrNotFound(err)
	}
	return file, entry, nil
}

func (s *Service) resolve(requestPath string) (string, string, error) {
	if !s.Enabled() {
		return "", "", ErrDisabled
	}
	clean, err := cleanPath(requestPath)
	if err != nil {
		return "", "", err
	}

	rootInfo, err := os.Stat(s.root)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !rootInfo.IsDir() {
		return "", "", fmt.Errorf("%w: configured path is not a directory", ErrUnavailable)
	}
	root, err := realPath(s.root)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	relative := strings.TrimPrefix(clean, "/")
	absolute := s.root
	if relative != "" {
		absolute = s.root + string(os.PathSeparator) + strings.ReplaceAll(relative, "/", string(os.PathSeparator))
	}

	// The root itself is allowed to be a symlink, but children are not. This
	// makes the configured root convenient while keeping the exposed tree
	// closed under its original directory.
	if relative != "" {
		info, err := os.Lstat(absolute)
		if err != nil {
			return "", "", unavailableOrNotFound(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", ErrNotFound
		}
	}
	target, err := realPath(absolute)
	if err != nil {
		return "", "", unavailableOrNotFound(err)
	}
	if !within(root, target) {
		return "", "", ErrNotFound
	}
	return clean, absolute, nil
}

func cleanPath(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	if strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("%w: contains an unsupported character", ErrInvalidPath)
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: path escapes the configured root", ErrInvalidPath)
		}
	}
	clean := pathpkg.Clean(raw)
	if clean == "." || clean == "" {
		return "/", nil
	}
	return clean, nil
}

func childPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func realPath(name string) (string, error) {
	return filepath.EvalSymlinks(name)
}

func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func unavailableOrNotFound(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func sizeOf(info os.FileInfo) int64 {
	if info.IsDir() {
		return 0
	}
	return info.Size()
}
