package drive

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dibin/tdrive/internal/database"
)

// A scope confines an account to one subtree of the drive.
//
// The drive itself stays a single tree — one Telegram account, one channel, one
// namespace — because that is what makes the index rebuildable and what makes a
// segment's caption meaningful on its own. A scope is a lens over that tree,
// applied at the edges: paths coming in from a client are rewritten into real
// drive paths, and paths going back out are rewritten into the client's view.
//
// Doing the translation at the boundary rather than deep in the service is
// deliberate. Every mutation below this line keeps working on real paths, so
// there is no possibility of a code path that forgets to apply the scope and
// quietly operates on the wrong tree; the worst a missed call site can do is
// show an absolute path where a relative one was expected.

// ErrOutOfScope is returned when a request names a path outside the account's
// subtree. It maps onto 404 rather than 403: confirming that a path exists but
// is off-limits tells the caller about a part of the drive they were not meant
// to know about.
var ErrOutOfScope = errors.New("drive: no such file or directory")

// ErrQuotaExceeded is returned when an upload would push an account past its
// storage allowance.
var ErrQuotaExceeded = errors.New("drive: storage quota exceeded")

// Scope is an account's view of the drive. The zero Scope is the whole drive.
type Scope struct {
	// Root is a clean real path, or empty for the whole drive.
	Root string
}

// ScopeOf reads the scope out of an account. Administrators are never scoped:
// somebody has to be able to see the whole drive, and an admin who could not
// would have no way to notice a file stranded outside every scope.
func ScopeOf(u database.User) Scope {
	if u.Role == database.RoleAdmin || u.ScopePath == "" {
		return Scope{}
	}
	clean, err := CleanPath(u.ScopePath)
	if err != nil || clean == Root {
		return Scope{}
	}
	return Scope{Root: clean}
}

// Unrestricted reports whether the scope is the whole drive, which lets hot
// paths skip the translation entirely.
func (s Scope) Unrestricted() bool { return s.Root == "" }

// ToReal converts a path as the client sees it into a real drive path.
func (s Scope) ToReal(p string) (string, error) {
	clean, err := CleanPath(p)
	if err != nil {
		return "", err
	}
	if s.Unrestricted() {
		return clean, nil
	}
	if clean == Root {
		return s.Root, nil
	}
	return s.Root + clean, nil
}

// ToVisible converts a real drive path into the client's view. The second
// result is false when the path lies outside the scope, which callers treat as
// "does not exist".
func (s Scope) ToVisible(real string) (string, bool) {
	if s.Unrestricted() {
		return real, true
	}
	if real == s.Root {
		return Root, true
	}
	if !strings.HasPrefix(real, s.Root+"/") {
		return "", false
	}
	return strings.TrimPrefix(real, s.Root), true
}

// Contains reports whether a real path is inside the scope.
func (s Scope) Contains(real string) bool {
	_, ok := s.ToVisible(real)
	return ok
}

// Apply rewrites an entry's path into the client's view, reporting false for
// entries the client should not be shown at all.
func (s Scope) Apply(e Entry) (Entry, bool) {
	if s.Unrestricted() {
		return e, true
	}
	visible, ok := s.ToVisible(e.Path)
	if !ok {
		return Entry{}, false
	}
	e.Path = visible
	return e, true
}

// ApplyAll filters and rewrites a listing.
func (s Scope) ApplyAll(entries []Entry) []Entry {
	if s.Unrestricted() {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if scoped, ok := s.Apply(e); ok {
			out = append(out, scoped)
		}
	}
	return out
}

// EnsureScopeRoot creates an account's subtree if it does not exist yet, so
// that a freshly scoped account can browse and upload immediately instead of
// meeting an error at a directory nobody has made.
func (s *Service) EnsureScopeRoot(ctx context.Context, scope Scope) error {
	if scope.Unrestricted() {
		return nil
	}
	if _, err := s.db.DirByPath(ctx, scope.Root); err == nil {
		return nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return err
	}
	_, err := s.Mkdir(ctx, scope.Root)
	return err
}

// CheckQuota reports whether an account can take on additional bytes.
//
// The check is deliberately advisory rather than transactional: two uploads
// started in the same instant can both pass and together exceed the quota by
// one file. Making it exact would mean holding a lock across a multi-gigabyte
// transfer, which is a far worse trade than occasionally going slightly over.
// Pending files count toward usage, so the overshoot is bounded by what is in
// flight rather than unbounded.
func (s *Service) CheckQuota(ctx context.Context, userID string, additional int64) error {
	if userID == "" || additional <= 0 {
		return nil
	}
	user, err := s.db.UserByID(ctx, userID)
	if err != nil {
		// A transfer started by the server has no account, and an account that
		// vanished mid-upload is not a reason to fail the bytes already in
		// flight.
		if errors.Is(err, database.ErrNotFound) {
			return nil
		}
		return err
	}
	if user.QuotaBytes <= 0 {
		return nil
	}

	used, err := s.db.UsedBytesByOwner(ctx, userID)
	if err != nil {
		return err
	}
	if used+additional > user.QuotaBytes {
		return fmt.Errorf("%w: %s would need %d bytes but only %d of %d remain",
			ErrQuotaExceeded, user.Username, additional, max(user.QuotaBytes-used, 0), user.QuotaBytes)
	}
	return nil
}
