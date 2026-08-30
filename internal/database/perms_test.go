package database

import "testing"

// Permissions are stored as a bitmask but a zero mask is not "no permissions":
// it means the account follows its role. Getting that backwards would silently
// lock out every account that existed before permissions did.
func TestEffectivePermissions(t *testing.T) {
	cases := []struct {
		name  string
		user  User
		want  Perm
		check []struct {
			perm    Perm
			allowed bool
		}
	}{
		{
			name: "an account with no stored mask follows its role",
			user: User{Role: RoleUser, Perms: 0},
			want: DefaultUserPerms,
			check: []struct {
				perm    Perm
				allowed bool
			}{
				{PermRead, true},
				{PermUpload, true},
				{PermDelete, true},
				{PermWebDAV, true},
				// The permissions that spend a server-side resource are not
				// part of the default set.
				{PermUploadLocal, false},
				{PermRemoteFetch, false},
				{PermStage, false},
			},
		},
		{
			name: "an administrator holds everything regardless of the mask",
			user: User{Role: RoleAdmin, Perms: PermRead},
			want: AllPerms,
			check: []struct {
				perm    Perm
				allowed bool
			}{
				{PermDelete, true},
				{PermStage, true},
				{PermRemoteFetch, true},
			},
		},
		{
			name: "a customised account gets exactly what was stored, plus read",
			user: User{Role: RoleUser, Perms: PermDownload},
			// Read is implied: every other permission has to resolve a path
			// first, so an account without it could not use what it does have.
			want: PermDownload | PermRead,
			check: []struct {
				perm    Perm
				allowed bool
			}{
				{PermRead, true},
				{PermDownload, true},
				{PermUpload, false},
				{PermDelete, false},
				{PermWebDAV, false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.user.Effective(); got != tc.want {
				t.Errorf("Effective() = %v (%v), want %v (%v)",
					got, PermNames(got), tc.want, PermNames(tc.want))
			}
			for _, c := range tc.check {
				if got := tc.user.Can(c.perm); got != c.allowed {
					t.Errorf("Can(%v) = %v, want %v", PermNames(c.perm), got, c.allowed)
				}
			}
		})
	}
}

// The API and the WebUI speak permission names, not bit positions, so the
// mapping has to survive a round trip in both directions.
func TestPermNameRoundTrip(t *testing.T) {
	names := PermNames(AllPerms)
	if len(names) == 0 {
		t.Fatal("PermNames(AllPerms) returned nothing")
	}
	if got := PermsFromNames(names); got != AllPerms {
		t.Fatalf("round trip lost bits: %v, want %v", got, AllPerms)
	}

	// A name from a newer build must not poison the rest of the request.
	mixed := PermsFromNames([]string{"read", "download", "teleport"})
	if mixed != PermRead|PermDownload {
		t.Fatalf("unknown name changed the result: %v", PermNames(mixed))
	}
}
