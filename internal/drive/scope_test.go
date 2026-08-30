package drive

import (
	"testing"

	"github.com/dibin/tdrive/internal/database"
)

// A scope is the only thing standing between a confined account and the rest
// of the drive, so the translation has to be exact in both directions and has
// to refuse everything outside the subtree.
func TestScopeTranslatesPaths(t *testing.T) {
	scope := ScopeOf(database.User{Role: database.RoleUser, ScopePath: "/users/alice"})
	if scope.Unrestricted() {
		t.Fatal("a user with a scope path should be restricted")
	}

	cases := []struct {
		visible string
		real    string
	}{
		{"/", "/users/alice"},
		{"/movies", "/users/alice/movies"},
		{"/movies/2024/a.mkv", "/users/alice/movies/2024/a.mkv"},
	}
	for _, tc := range cases {
		got, err := scope.ToReal(tc.visible)
		if err != nil {
			t.Fatalf("ToReal(%q): %v", tc.visible, err)
		}
		if got != tc.real {
			t.Errorf("ToReal(%q) = %q, want %q", tc.visible, got, tc.real)
		}
		back, ok := scope.ToVisible(tc.real)
		if !ok || back != tc.visible {
			t.Errorf("ToVisible(%q) = %q, %v; want %q, true", tc.real, back, ok, tc.visible)
		}
	}
}

func TestScopeRejectsPathsOutsideIt(t *testing.T) {
	scope := ScopeOf(database.User{Role: database.RoleUser, ScopePath: "/users/alice"})

	outside := []string{
		"/",
		"/users",
		"/users/bob",
		"/users/bob/secret.mkv",
		// A sibling whose name merely starts with the scope must not match:
		// this is the prefix bug that a plain HasPrefix would introduce.
		"/users/alice2",
		"/users/alice2/thing.mkv",
	}
	for _, real := range outside {
		if scope.Contains(real) {
			t.Errorf("Contains(%q) = true, want false", real)
		}
	}

	// Climbing out with .. must resolve back inside rather than escaping.
	got, err := scope.ToReal("/../../etc/passwd")
	if err != nil {
		t.Fatalf("ToReal on a traversal attempt: %v", err)
	}
	if !scope.Contains(got) {
		t.Fatalf("a traversal escaped the scope: %q", got)
	}
}

// Somebody has to be able to see the whole drive, or a file stranded outside
// every scope would be invisible to everyone.
func TestAdministratorsAreNeverScoped(t *testing.T) {
	scope := ScopeOf(database.User{Role: database.RoleAdmin, ScopePath: "/users/alice"})
	if !scope.Unrestricted() {
		t.Fatal("an administrator should never be confined to a subtree")
	}
	if !scope.Contains("/anything/at/all") {
		t.Fatal("an unrestricted scope should contain every path")
	}
}

func TestScopeFiltersListings(t *testing.T) {
	scope := ScopeOf(database.User{Role: database.RoleUser, ScopePath: "/users/alice"})
	entries := []Entry{
		{Name: "movies", Path: "/users/alice/movies", IsDir: true},
		{Name: "secret", Path: "/users/bob/secret", IsDir: true},
		{Name: "a.mkv", Path: "/users/alice/a.mkv"},
	}

	got := scope.ApplyAll(entries)
	if len(got) != 2 {
		t.Fatalf("kept %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Path != "/movies" || got[1].Path != "/a.mkv" {
		t.Fatalf("paths were not rewritten into the account's view: %+v", got)
	}
}
