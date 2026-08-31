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

// Aborted reports whether a job stopped short of storing its file. A segment
// that lands after this point belongs to a transfer nobody is waiting for any
// more, and writing it would move the row back to running — which is how a
// cancelled upload used to reappear as if it were still going.
func (s JobStatus) Aborted() bool { return s == JobFailed || s == JobCancelled }

// Terminal reports whether a job has reached a state it never leaves.
func (s JobStatus) Terminal() bool { return s == JobComplete || s.Aborted() }

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`

	Enabled bool `json:"enabled"`
	// Perms is the raw stored mask. Zero means "follow the role", so callers
	// must go through Effective or Can rather than testing it directly. The
	// JSON form is the name list, which is what the API and WebUI speak.
	Perms Perm `json:"-"`
	// ScopePath confines the account to one subtree; empty is the whole drive.
	ScopePath string `json:"scopePath"`
	// QuotaBytes caps the total size of files this account owns; 0 disables.
	QuotaBytes  int64     `json:"quotaBytes"`
	Note        string    `json:"note"`
	LastLoginAt time.Time `json:"-"`
	LastLoginIP string    `json:"lastLoginIp,omitempty"`
}

// Session is one live refresh token, described well enough for a person to
// recognise which of their devices it is.
type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	UserAgent  string    `json:"userAgent"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// TGAccount is one Telegram login. A deployment may hold several so that the
// per-account FLOOD_WAIT and transfer budgets add up instead of contending;
// each one carries its own api credentials and its own session file.
type TGAccount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	AppID int    `json:"appId"`
	// AppHash is a credential and is only exposed to administrators, so it
	// rides along here and is stripped by the API layer where appropriate.
	AppHash string `json:"appHash"`
	// ProxyURL routes this account's Telegram traffic through a SOCKS5 or HTTP
	// proxy, empty meaning a direct connection. It is per account because that
	// is the point: Telegram associates logins that share an exit address, so
	// several accounts behind one IP invite exactly the risk control that a
	// second account was added to avoid.
	//
	// It may embed a password, so it is never serialised directly; the API
	// layer publishes a masked form instead.
	ProxyURL string `json:"-"`
	// SessionFile is relative to the data directory.
	SessionFile string `json:"-"`
	Enabled     bool   `json:"enabled"`
	IsPrimary   bool   `json:"isPrimary"`
	// TGUserID, Username and Phone are cached from the last successful login so
	// the accounts list can name an account without a live connection.
	TGUserID  int64     `json:"tgUserId,omitempty"`
	Username  string    `json:"username,omitempty"`
	Phone     string    `json:"phone,omitempty"`
	Position  int       `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}

// ChannelAccess is one account's view of a storage channel. Telegram mints
// access hashes per account, so each account holds a different value for the
// same channel and an account without a row here has never resolved it.
type ChannelAccess struct {
	ChannelID  string
	AccountID  string
	AccessHash int64
	CanPost    bool
	CheckedAt  time.Time
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
	OwnerID   string    `json:"ownerId,omitempty"`
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
	// OwnerID is who uploaded the file. It is also written into the Telegram
	// caption, so a rebuilt index restores it rather than zeroing everyone's
	// quota usage.
	OwnerID string `json:"ownerId,omitempty"`
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
	// AccountID is the Telegram account that uploaded this segment, and
	// therefore the only account for which AccessHash and FileReference above
	// are valid — Telegram mints both per account. Any other account has to
	// re-resolve its own handle from TGMsgID before reading. Empty means
	// unknown: rows written before multiple accounts existed, and rows
	// recovered by an index rebuild.
	AccountID string `json:"-"`
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
	// StartedAt and FinishedAt bracket the time bytes were actually moving,
	// which is what an average speed has to be divided by. A job that waited
	// in the concurrency queue must not have that wait counted against it.
	StartedAt  time.Time `json:"-"`
	FinishedAt time.Time `json:"-"`
}

// DownloadMode is how a download's bytes reach the client.
type DownloadMode string

const (
	// DownloadDirect streams straight through the server from Telegram.
	DownloadDirect DownloadMode = "direct"
	// DownloadStaged assembles the whole file on the server's disk first, then
	// serves it locally. It is the only mode that makes a many-segment file
	// safe to pull with a parallel downloader.
	DownloadStaged DownloadMode = "staged"
	// DownloadSegments hands the client one link per stored segment and lets
	// it join them locally.
	DownloadSegments DownloadMode = "segments"
	// DownloadWebDAV is a mounted client reading a file. Nothing on the server
	// asked for it and nothing reports when it is done, so the row is opened
	// and settled by the read session itself; it exists so that a 40 GB pull
	// through /dav is visible in the transfer panel like any other transfer.
	DownloadWebDAV DownloadMode = "webdav"
)

// DownloadStatus is the lifecycle of a download job. Staged jobs stop at
// ready — the bytes are on disk and waiting — and only reach complete once the
// client has actually taken them.
type DownloadStatus string

const (
	DownloadPending   DownloadStatus = "pending"
	DownloadRunning   DownloadStatus = "running"
	DownloadReady     DownloadStatus = "ready"
	DownloadComplete  DownloadStatus = "complete"
	DownloadFailed    DownloadStatus = "failed"
	DownloadCancelled DownloadStatus = "cancelled"
	DownloadExpired   DownloadStatus = "expired"
)

// DownloadJob mirrors UploadJob for the other direction.
type DownloadJob struct {
	ID              string         `json:"id"`
	UserID          string         `json:"-"`
	FileID          string         `json:"fileId,omitempty"`
	Name            string         `json:"name"`
	TotalSize       int64          `json:"totalSize"`
	DownloadedBytes int64          `json:"downloadedBytes"`
	Mode            DownloadMode   `json:"mode"`
	Status          DownloadStatus `json:"status"`
	Error           string         `json:"error,omitempty"`
	CachePath       string         `json:"-"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	StartedAt       time.Time      `json:"-"`
	FinishedAt      time.Time      `json:"-"`
	ExpiresAt       time.Time      `json:"-"`
	LastUsedAt      time.Time      `json:"-"`
}

// PluginRecord is the local installation metadata for one plugin. Unlike the
// drive tree this data is not reconstructed from Telegram; it describes the
// executable and manifest selected by the administrator on this host.
type PluginRecord struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Version        string    `json:"version"`
	Author         string    `json:"author"`
	Enabled        bool      `json:"enabled"`
	Status         string    `json:"status"`
	Source         string    `json:"source"`
	ManifestURL    string    `json:"manifestUrl,omitempty"`
	ManifestDigest string    `json:"manifestDigest"`
	BinaryDigest   string    `json:"binaryDigest"`
	BinaryPath     string    `json:"-"`
	ManifestJSON   string    `json:"-"`
	Error          string    `json:"error,omitempty"`
	InstalledAt    time.Time `json:"installedAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

const (
	PluginStatusActive   = "active"
	PluginStatusDisabled = "disabled"
	PluginStatusError    = "error"
	PluginStatusStopped  = "stopped"
)

// ShareKind distinguishes a link to a whole file from a link to one stored
// segment of it.
type ShareKind string

const (
	ShareFile    ShareKind = "file"
	ShareSegment ShareKind = "segment"
)

// ShareLink is a durable, revocable capability to read one file's bytes.
// Token is only populated at creation time; afterwards only the hash exists.
type ShareLink struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	FileID     string    `json:"fileId"`
	Kind       ShareKind `json:"kind"`
	Label      string    `json:"label,omitempty"`
	ExpiresAt  time.Time `json:"-"`
	Revoked    bool      `json:"revoked"`
	Hits       int64     `json:"hits"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"-"`
}

// AuditEntry is one recorded administrative action.
type AuditEntry struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	ActorID   string    `json:"actorId,omitempty"`
	ActorName string    `json:"actorName"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	IP        string    `json:"ip,omitempty"`
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
