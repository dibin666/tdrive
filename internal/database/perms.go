package database

import "strings"

// Perm is one thing an account is allowed to do.
//
// Roles decide who can reconfigure the drive; permissions decide what an
// ordinary account may do with the files. The two are deliberately separate:
// "can delete" and "can change the Telegram credentials" are not the same
// question, and collapsing them is what forces deployments to hand out admin
// accounts they did not want to hand out.
type Perm uint32

const (
	// PermRead is the floor: browse listings, stat, and preview. Without it an
	// account can log in and see nothing, which is the honest meaning of an
	// account that exists but has been switched off feature by feature.
	PermRead Perm = 1 << iota
	// PermDownload covers reading a file's bytes by any route: the raw
	// endpoint, a share link, a staged download, WebDAV GET.
	PermDownload
	// PermUpload is a browser-driven upload.
	PermUpload
	// PermUploadLocal is an upload from the server's own mounted directory. It
	// is separate from PermUpload because it lets an account reach files on
	// the host that it never sent itself.
	PermUploadLocal
	// PermRemoteFetch is the offline downloader: the server fetches a URL. It
	// makes the server issue outbound requests on the account's behalf, which
	// is worth gating on its own.
	PermRemoteFetch
	PermMkdir
	// PermRename also covers batch rename.
	PermRename
	PermMove
	PermDelete
	// PermWebDAV allows these credentials to be used over WebDAV at all.
	PermWebDAV
	// PermStage allows starting a VPS-staged download, which consumes server
	// disk rather than only bandwidth.
	PermStage
	// PermShare allows minting durable, reusable download links.
	PermShare
	// PermPlugins allows installing, updating, enabling and removing this
	// account's own plugins.
	//
	// It is deliberately absent from DefaultUserPerms. A plugin is a
	// standalone executable that runs as a child of tdrive with the tdrive
	// process's full privileges (see internal/plugin/manager.go). Per-account
	// ownership decides whose data and whose traffic a plugin sees; it does
	// not sandbox what a plugin can ask the host to do. Granting this is
	// granting code execution, not a feature toggle.
	PermPlugins
)

// permNames is the wire vocabulary. The bitmask is a storage detail; the API
// and the WebUI speak these strings, so adding a permission never silently
// shifts the meaning of a stored number.
var permNames = []struct {
	Perm Perm
	Name string
}{
	{PermRead, "read"},
	{PermDownload, "download"},
	{PermUpload, "upload"},
	{PermUploadLocal, "uploadLocal"},
	{PermRemoteFetch, "remoteFetch"},
	{PermMkdir, "mkdir"},
	{PermRename, "rename"},
	{PermMove, "move"},
	{PermDelete, "delete"},
	{PermWebDAV, "webdav"},
	{PermStage, "stage"},
	{PermShare, "share"},
	{PermPlugins, "plugins"},
}

// AllPerms is every permission set at once, which is what an administrator
// effectively holds.
const AllPerms = PermRead | PermDownload | PermUpload | PermUploadLocal |
	PermRemoteFetch | PermMkdir | PermRename | PermMove | PermDelete |
	PermWebDAV | PermStage | PermShare | PermPlugins

// DefaultUserPerms is what a plain account gets when nothing has been
// customised. It matches exactly what every account could do before
// permissions existed, so upgrading changes nobody's access.
//
// PermUploadLocal, PermRemoteFetch and PermStage are excluded: each of them
// spends a server-side resource (the host filesystem, outbound bandwidth,
// disk) rather than only the account's own bytes. PermPlugins is excluded for
// a stronger reason — it runs a chosen executable with tdrive's privileges,
// so it is admin-only until somebody deliberately hands it out.
const DefaultUserPerms = PermRead | PermDownload | PermUpload | PermMkdir |
	PermRename | PermMove | PermDelete | PermWebDAV | PermShare

// DefaultPerms returns the permissions implied by a role.
func DefaultPerms(role Role) Perm {
	if role == RoleAdmin {
		return AllPerms
	}
	return DefaultUserPerms
}

// PermsFromNames parses the wire form. Unknown names are ignored rather than
// rejected so that a WebUI from a newer build cannot lock an admin out of
// saving the rest of a form.
func PermsFromNames(names []string) Perm {
	var out Perm
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		for _, p := range permNames {
			if p.Name == name {
				out |= p.Perm
			}
		}
	}
	return out
}

// PermNames renders a mask as the wire form, in declaration order so the
// output is stable.
func PermNames(p Perm) []string {
	out := make([]string, 0, len(permNames))
	for _, entry := range permNames {
		if p&entry.Perm != 0 {
			out = append(out, entry.Name)
		}
	}
	return out
}

// AllPermNames lists every permission the build knows, in declaration order,
// so the WebUI can render a form without hardcoding the list.
func AllPermNames() []string { return PermNames(AllPerms) }

// Effective resolves what an account may actually do.
//
// A stored zero means "follow the role", which is how accounts that predate
// per-user permissions keep working. An administrator always holds everything:
// an admin who could revoke their own ability to read would be a footgun with
// no upside, and admins can edit the permission fields anyway.
func (u User) Effective() Perm {
	if u.Role == RoleAdmin {
		return AllPerms
	}
	if u.Perms == 0 {
		return DefaultPerms(u.Role)
	}
	// PermRead is implied by holding any permission at all: every other
	// permission needs to resolve a path first.
	return u.Perms | PermRead
}

// Can reports whether the account holds a permission.
func (u User) Can(p Perm) bool { return u.Effective()&p == p }
