// Package drive is the service layer: it turns paths into Telegram messages and
// back, owns the segmented upload pipeline, and supplies the byte sources that
// internal/reader stitches together.
//
// It is the only place that knows both halves of the design — the SQLite index
// and the tagged captions — and it keeps them in step: every mutation writes
// Telegram first, then the index, so a crash leaves a recoverable channel
// rather than an index pointing at messages that were never sent.
package drive

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/dibin/tdrive/internal/tagcodec"
)

// Root is the drive's root path.
const Root = "/"

// ErrInvalidPath and friends are returned as-is to the API layer, which maps
// them onto 400 rather than 500.
type PathError struct {
	Path   string
	Reason string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("invalid path %q: %s", e.Path, e.Reason)
}

// CleanPath normalises a client-supplied path into the canonical form stored in
// the dirs table: absolute, slash-separated, no trailing slash, no "." or ".."
// components left.
//
// WebDAV clients send percent-decoded paths that may contain redundant slashes
// or a trailing one depending on whether they think the target is a collection,
// so this has to be forgiving about shape while still rejecting anything that
// could escape the drive.
func CleanPath(p string) (string, error) {
	if p == "" {
		return Root, nil
	}
	if strings.ContainsRune(p, 0) {
		return "", &PathError{Path: p, Reason: "contains a NUL byte"}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == "" {
		cleaned = Root
	}
	// path.Clean resolves ".." lexically, so a path that tried to climb out of
	// the root comes back rooted. Catching the leftover form is belt and
	// braces against a client sending something Clean cannot fold.
	if strings.HasPrefix(cleaned, "..") {
		return "", &PathError{Path: p, Reason: "escapes the drive root"}
	}

	for _, name := range SplitPath(cleaned) {
		if err := ValidateName(name); err != nil {
			return "", err
		}
	}
	return cleaned, nil
}

// SplitPath returns the path components, empty for the root.
func SplitPath(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// Join builds a child path. parent must already be clean.
func Join(parent, name string) string {
	if parent == Root || parent == "" {
		return Root + name
	}
	return parent + "/" + name
}

// Parent returns the containing directory path and the final component.
func Parent(p string) (dir, name string) {
	if p == Root || p == "" {
		return Root, ""
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return Root, p[1:]
	}
	return p[:i], p[i+1:]
}

// ValidateName rejects names that cannot round-trip through a Telegram caption,
// a WebDAV XML response or a filesystem.
func ValidateName(name string) error {
	switch {
	case name == "":
		return &PathError{Path: name, Reason: "empty name"}
	case name == "." || name == "..":
		return &PathError{Path: name, Reason: "reserved name"}
	case strings.ContainsRune(name, '/'):
		return &PathError{Path: name, Reason: "contains a slash"}
	case len(name) > tagcodec.MaxNameBytes:
		// The limit exists so that #n_, the base32 of the name, always fits
		// inside a caption alongside the machine tags.
		return &PathError{
			Path:   name,
			Reason: fmt.Sprintf("is %d bytes; the limit is %d", len(name), tagcodec.MaxNameBytes),
		}
	}

	for _, r := range name {
		// Control characters would break the XML body of a PROPFIND response
		// and cannot be represented in a caption line.
		if r < 0x20 || r == 0x7f || (unicode.IsControl(r) && r != '\t') {
			return &PathError{Path: name, Reason: "contains a control character"}
		}
	}
	return nil
}

// IsDescendant reports whether child lies inside parent. Moving a directory
// into its own subtree would detach it from the root, so the move path checks
// this before touching Telegram.
func IsDescendant(parent, child string) bool {
	if parent == Root {
		return child != Root
	}
	return strings.HasPrefix(child, parent+"/")
}

// AncestorNames lists the directory names from the given path outwards, nearest
// first. These become the readable hashtags on a file's caption, which is what
// makes "#电影" work as a folder filter inside a Telegram client.
func AncestorNames(dirPath string) []string {
	parts := SplitPath(dirPath)
	out := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		out = append(out, parts[i])
	}
	return out
}
