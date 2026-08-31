package drive

import (
	"context"
	"io"
)

// Backend is the set of Telegram operations the drive needs.
//
// It exists as an interface for one reason: the segmentation logic — splitting
// an upload across objects, mapping a byte range back onto them, keeping
// captions in step through renames — is the part of this program most likely
// to have an off-by-one that silently corrupts a file, and it cannot be tested
// against the real Telegram in CI. With this seam the whole path from "store a
// 5 GB file" to "read bytes 4999999000-5000001000 back" runs in a unit test.
//
// internal/tgc provides the real implementation.
type Backend interface {
	// Ready reports whether the Telegram session can serve requests.
	Ready() bool

	// Upload streams size bytes and posts them as a document, returning the
	// coordinates needed to read them back.
	Upload(ctx context.Context, ch ChannelRef, spec UploadSpec) (StoredDoc, error)

	// SendRecord posts a caption-only message, used for directory records and
	// for empty files.
	SendRecord(ctx context.Context, ch ChannelRef, caption string) (int, error)

	// EditRecord rewrites a message's caption, which is how a rename or move
	// reaches the durable copy of a file's name and location.
	EditRecord(ctx context.Context, ch ChannelRef, msgID int, caption string) error

	// DeleteRecords removes messages.
	DeleteRecords(ctx context.Context, ch ChannelRef, msgIDs []int) error

	// ReadChunk reads a byte range of a stored document. offset and limit are
	// already aligned so the read stays inside one 1 MiB window.
	ReadChunk(ctx context.Context, ch ChannelRef, doc StoredDoc, offset, limit int64) ([]byte, error)

	// RefreshDoc re-resolves a document after its file reference expired.
	RefreshDoc(ctx context.Context, ch ChannelRef, msgID int) (StoredDoc, error)

	// IsReferenceExpired reports whether a read failed because the cached
	// file reference went stale, which is recoverable via RefreshDoc.
	IsReferenceExpired(err error) bool

	// MigratedDC reports the datacenter a document actually lives on when a
	// read failed because it was looked for on the wrong one.
	MigratedDC(err error) (int, bool)
}

// Account is one Telegram login. Every Backend call belongs to exactly one of
// them, because Telegram meters its limits per account and — more awkwardly —
// mints access hashes per account too: the coordinates one account holds for a
// channel or a document are meaningless to another.
type Account interface {
	Backend

	// ID identifies the account in the segments table and in the scheduler.
	ID() string

	// Available reports whether the scheduler should hand this account new
	// work: signed in, admitted to the storage channel, and not sitting out a
	// FLOOD_WAIT.
	Available() bool

	// ChannelRef resolves this account's own coordinates for a stored channel.
	// It fails for an account that has never been admitted to the channel,
	// which is the correct answer: it cannot read or write there.
	ChannelRef(ctx context.Context, channelID string) (ChannelRef, error)
}

// Cluster is the set of accounts a deployment has. Which one a given transfer
// runs on is decided in this package rather than here, because the choice is
// inseparable from the task slot it comes with.
type Cluster interface {
	// Ready reports whether any account can serve requests.
	Ready() bool

	// Accounts lists the accounts eligible for new work, in a stable order.
	Accounts() []Account

	// Account looks one up by id, including accounts that are currently
	// throttled or signed out.
	Account(id string) (Account, bool)
}

// ChannelRef identifies a storage channel, as seen by one particular account.
type ChannelRef struct {
	// ChannelID is the local database row. Telegram does not need it, but it
	// lets a backend persist a freshly resolved access hash after Telegram
	// rejects a stale one.
	ChannelID string
	TGID      int64
	// AccessHash is minted per account. Using one account's value with another
	// account's connection fails with CHANNEL_INVALID.
	AccessHash int64
}

// StoredDoc is everything needed to read a segment back. It is exactly what a
// row in the segments table holds.
type StoredDoc struct {
	MsgID      int
	DocID      int64
	AccessHash int64
	// DCID is the datacenter holding the document; reading from the wrong one
	// fails with FILE_MIGRATE.
	DCID int
	// FileReference is Telegram's anti-hotlinking token, valid for about an
	// hour. A stale one here is normal, not an error.
	FileReference []byte
	Size          int64
}

// UploadSpec describes one document to store.
type UploadSpec struct {
	// Name is the filename attribute; for a split file it carries a .partNNN
	// suffix so a channel browsed in a Telegram client reads in order.
	Name string
	MIME string
	// Caption is the tagged record built by internal/tagcodec.
	Caption string
	Body    io.Reader
	// Size must be exact. Telegram commits to a part count in the first
	// upload request and cannot be corrected afterwards.
	Size int64
	// Progress, if set, is called with the running confirmed byte count.
	Progress func(uploaded, total int64)
}
