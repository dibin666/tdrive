package database

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// Role controls what a WebUI account may do. Every account sees the same drive;
// only an admin can change the Telegram binding, manage users or rebuild the
// index.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// FileStatus tracks whether every segment of a file is actually present.
type FileStatus string

const (
	// StatusPending means an upload is still in flight. Pending files are
	// hidden from listings and from WebDAV.
	StatusPending FileStatus = "pending"
	// StatusComplete means all segments are stored.
	StatusComplete FileStatus = "complete"
	// StatusBroken means the indexer found a file whose segments do not add
	// up. These stay visible with a warning: silently hiding a partially
	// recoverable multi-gigabyte file is worse than showing it.
	StatusBroken FileStatus = "broken"
)

// JobStatus is the lifecycle of an upload job.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobComplete  JobStatus = "complete"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Channel is a Telegram channel used as a storage backend. The default channel
// receives new uploads; others stay readable so that switching channels does
// not orphan existing files.
type Channel struct {
	ID         string    `json:"id"`
	TGID       int64     `json:"tgId"`
	AccessHash int64     `json:"-"`
	Title      string    `json:"title"`
	IsDefault  bool      `json:"isDefault"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Dir is a directory. ParentID is empty at the drive root, matching
// tagcodec.RootParent.
type Dir struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId,omitempty"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	ChannelID string    `json:"-"`
	TGMsgID   int       `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// File is one logical file. Size is the whole file, not one segment; callers
// outside internal/reader and internal/uploader should never need to think
// about segments at all.
type File struct {
	ID           string     `json:"id"`
	DirID        string     `json:"dirId,omitempty"`
	Name         string     `json:"name"`
	Size         int64      `json:"size"`
	MIME         string     `json:"mime"`
	SegmentSize  int64      `json:"-"`
	SegmentCount int        `json:"segmentCount"`
	Status       FileStatus `json:"status"`
	ChannelID    string     `json:"-"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Segment is one Telegram document backing part of a file. Index is 1-based to
// match the #seg_i_n caption tag.
type Segment struct {
	FileID     string `json:"-"`
	Index      int    `json:"index"`
	Size       int64  `json:"size"`
	TGMsgID    int    `json:"-"`
	TGDocID    int64  `json:"-"`
	AccessHash int64  `json:"-"`
	// DCID is the datacenter holding the document. Reading from the wrong one
	// fails with FILE_MIGRATE, so the reader routes by this value.
	DCID int `json:"-"`
	// FileReference is Telegram's anti-hotlinking token. It expires after
	// roughly an hour, so a stale value here is normal and readers refresh it
	// rather than treating it as an error.
	FileReference []byte `json:"-"`
}

// UploadJob is the resumable state of one upload. DoneMask is a bitset over
// 1-based segment indices.
type UploadJob struct {
	ID            string    `json:"id"`
	UserID        string    `json:"-"`
	FileID        string    `json:"fileId,omitempty"`
	DirID         string    `json:"dirId,omitempty"`
	Name          string    `json:"name"`
	TotalSize     int64     `json:"totalSize"`
	SegmentSize   int64     `json:"segmentSize"`
	SegmentCount  int       `json:"segmentCount"`
	DoneMask      []byte    `json:"-"`
	UploadedBytes int64     `json:"uploadedBytes"`
	Status        JobStatus `json:"status"`
	Error         string    `json:"error,omitempty"`
	Source        string    `json:"source"`
	SourceURL     string    `json:"sourceUrl,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// PendingSegments lists the 1-based segment indices that still need uploading.
// The browser and the remote-URL fetcher both use it to resume.
func (j *UploadJob) PendingSegments() []int {
	var out []int
	for i := 1; i <= j.SegmentCount; i++ {
		if !MaskHas(j.DoneMask, i) {
			out = append(out, i)
		}
	}
	return out
}

// Done reports whether every segment has landed.
func (j *UploadJob) Done() bool {
	for i := 1; i <= j.SegmentCount; i++ {
		if !MaskHas(j.DoneMask, i) {
			return false
		}
	}
	return true
}

// NewMask allocates a bitset large enough for n 1-based indices.
func NewMask(n int) []byte {
	if n <= 0 {
		return nil
	}
	return make([]byte, (n+7)/8)
}

// MaskHas reports whether the 1-based index is set.
func MaskHas(mask []byte, idx int) bool {
	if idx < 1 {
		return false
	}
	b := (idx - 1) / 8
	if b >= len(mask) {
		return false
	}
	return mask[b]&(1<<uint((idx-1)%8)) != 0
}

// MaskSet sets the 1-based index, growing the bitset if needed.
func MaskSet(mask []byte, idx int) []byte {
	if idx < 1 {
		return mask
	}
	b := (idx - 1) / 8
	for len(mask) <= b {
		mask = append(mask, 0)
	}
	mask[b] |= 1 << uint((idx-1)%8)
	return mask
}

// NewID mints a ULID: time-ordered, 26 characters of Crockford base32, and
// therefore safe to embed directly in a Telegram hashtag.
func NewID() string {
	return ulid.MustNew(ulid.Now(), rand.Reader).String()
}

func msToTime(ms int64) time.Time { return time.UnixMilli(ms) }
