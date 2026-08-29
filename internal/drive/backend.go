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

// ChannelRef identifies a storage channel.
type ChannelRef struct {
	TGID       int64
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
