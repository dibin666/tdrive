// Package config loads tdrive's runtime configuration from environment
// variables, which is what a container deployment can actually set.
//
// Every knob has a default that works, so a first run only needs a data
// directory. The tuning defaults here are the ones the performance targets were
// written against: 512 KiB upload parts to match the reference implementation's
// MTProto part size, and a connection pool plus parallel chunk prefetch to beat
// its single-connection sequential download.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Telegram splits a big upload into at most 4000 parts of at most 512 KiB, and
// that product is exactly where the familiar "2 GB" free-account ceiling comes
// from. Everything about segmenting is derived from these two numbers.
const (
	// UploadPartSize is the MTProto part size for upload.saveBigFilePart.
	UploadPartSize = 512 * 1024
	// MaxUploadParts is the MTProto limit on file_total_parts.
	MaxUploadParts = 4000
	// TelegramFileLimit is the largest single object upload.saveBigFilePart
	// can express: 4000 parts of 512 KiB is 2000 MiB. Premium raises the size
	// a client may *send*, but not these two protocol limits, so this is the
	// hard ceiling for one segment either way. Storing more than this is what
	// segmenting is for.
	TelegramFileLimit int64 = MaxUploadParts * UploadPartSize

	// DefaultSegmentSize is 1900 MiB, exactly 3800 upload parts. The 100 MiB of
	// headroom under TelegramFileLimit keeps a segment from landing on the
	// boundary where Telegram starts rejecting uploads.
	DefaultSegmentSize int64 = 1900 * 1024 * 1024

	// DownloadChunkSize is the largest upload.getFile response Telegram will
	// return, and the unit the parallel reader prefetches in.
	DownloadChunkSize int64 = 1024 * 1024
)

// Config is the fully resolved configuration. Load validates it before
// returning, so nothing downstream needs to re-check these values.
type Config struct {
	Server   Server
	Auth     Auth
	Telegram Telegram
	Storage  Storage
	Stream   Stream
	WebDAV   WebDAV
	LogLevel string
}

type Server struct {
	// Listen is the HTTP bind address for the WebUI, REST API and WebDAV.
	Listen string
	// DataDir holds the SQLite database, the Telegram session and upload
	// spool files. It is the only path that needs to be a Docker volume.
	DataDir string
	// BaseURL is the externally reachable origin, used to render the WebDAV
	// address in the settings page. Empty means "derive from the request".
	BaseURL string
	// ReadHeaderTimeout guards against slowloris; body reads are deliberately
	// untimed because an upload segment legitimately takes minutes.
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
}

type Auth struct {
	// JWTSecret signs WebUI access tokens. Load generates and persists one on
	// first run so that a deployment without the env var still gets stable
	// sessions across restarts.
	JWTSecret []byte
	AccessTTL time.Duration
	// RefreshTTL bounds the HttpOnly refresh cookie stored in the database.
	RefreshTTL time.Duration
	// BootstrapUser and BootstrapPassword seed the first admin account when
	// the users table is empty. Without them the WebUI runs a setup wizard.
	BootstrapUser     string
	BootstrapPassword string
}

type Telegram struct {
	// AppID and AppHash come from my.telegram.org. They can also be supplied
	// through the setup wizard, in which case they are stored in settings.
	AppID   int
	AppHash string

	// PoolSize is how many MTProto connections are opened to a datacenter.
	// This is the single biggest lever on throughput: the reference
	// implementation streams over one connection, so a pool is where the Go
	// port gets ahead rather than merely keeping up.
	PoolSize int64

	// UploadThreads is the concurrency inside one segment upload, i.e. how
	// many saveBigFilePart calls are in flight at once.
	UploadThreads int

	// RateLimit smooths request bursts so Telegram is less likely to answer
	// with FLOOD_WAIT in the first place; the floodwait middleware handles the
	// ones that still happen.
	RateLimit time.Duration
	RateBurst int

	// SessionFile is derived from DataDir.
	SessionFile string
}

type Storage struct {
	// SegmentSize is where a logical file is split into separate Telegram
	// objects. Files at or below it become a single one-segment record, so
	// there is only ever one code path.
	SegmentSize int64
	// SegmentConcurrency is how many segments of one file upload at once.
	SegmentConcurrency int
	// SpoolDir holds request bodies whose length is unknown up front, which
	// in practice means WebDAV PUT from clients that use chunked encoding.
	SpoolDir string
	// DatabaseFile is derived from DataDir.
	DatabaseFile string
}

type Stream struct {
	// Concurrency is how many 1 MiB chunks are fetched in parallel while
	// serving a read. This is what replaces the reference implementation's
	// sequential 512 KiB iterator.
	Concurrency int
	// Buffers is the depth of the completed-chunk queue feeding the reader.
	Buffers int
	// ChunkTimeout bounds one upload.getFile call so a stalled datacenter
	// cannot wedge a download forever.
	ChunkTimeout time.Duration
	// LocationTTL is how long a resolved InputDocumentFileLocation is cached.
	// Telegram file references expire in roughly an hour; staying well under
	// that avoids most FILE_REFERENCE_EXPIRED round trips.
	LocationTTL time.Duration
}

type WebDAV struct {
	Enabled bool
	Prefix  string
}

// Load reads the environment and returns a validated configuration.
func Load() (*Config, error) {
	dataDir := envStr("TDRIVE_DATA_DIR", "")
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir %q: %w", dataDir, err)
	}
	dataDir = abs

	cfg := &Config{
		Server: Server{
			Listen:            envStr("TDRIVE_LISTEN", ":8080"),
			DataDir:           dataDir,
			BaseURL:           strings.TrimSuffix(envStr("TDRIVE_BASE_URL", ""), "/"),
			ReadHeaderTimeout: envDur("TDRIVE_READ_HEADER_TIMEOUT", 20*time.Second),
			ShutdownTimeout:   envDur("TDRIVE_SHUTDOWN_TIMEOUT", 30*time.Second),
		},
		Auth: Auth{
			AccessTTL:         envDur("TDRIVE_ACCESS_TTL", 15*time.Minute),
			RefreshTTL:        envDur("TDRIVE_REFRESH_TTL", 30*24*time.Hour),
			BootstrapUser:     envStr("TDRIVE_ADMIN_USER", ""),
			BootstrapPassword: envStr("TDRIVE_ADMIN_PASSWORD", ""),
		},
		Telegram: Telegram{
			AppHash:       envStr("TDRIVE_TG_APP_HASH", ""),
			PoolSize:      int64(envInt("TDRIVE_TG_POOL_SIZE", 8)),
			UploadThreads: envInt("TDRIVE_UPLOAD_THREADS", 8),
			RateLimit:     envDur("TDRIVE_TG_RATE_LIMIT", 100*time.Millisecond),
			RateBurst:     envInt("TDRIVE_TG_RATE_BURST", 5),
			SessionFile:   filepath.Join(dataDir, "session.json"),
		},
		Storage: Storage{
			SegmentSize:        envSize("TDRIVE_SEGMENT_SIZE", DefaultSegmentSize),
			SegmentConcurrency: envInt("TDRIVE_SEGMENT_CONCURRENCY", 2),
			SpoolDir:           filepath.Join(dataDir, "spool"),
			DatabaseFile:       filepath.Join(dataDir, "tdrive.db"),
		},
		Stream: Stream{
			Concurrency:  envInt("TDRIVE_STREAM_CONCURRENCY", 6),
			Buffers:      envInt("TDRIVE_STREAM_BUFFERS", 8),
			ChunkTimeout: envDur("TDRIVE_CHUNK_TIMEOUT", 30*time.Second),
			LocationTTL:  envDur("TDRIVE_LOCATION_TTL", 30*time.Minute),
		},
		WebDAV: WebDAV{
			Enabled: envBool("TDRIVE_WEBDAV_ENABLED", true),
			Prefix:  "/dav",
		},
		LogLevel: envStr("TDRIVE_LOG_LEVEL", "info"),
	}

	if v := os.Getenv("TDRIVE_TG_APP_ID"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("TDRIVE_TG_APP_ID: %w", err)
		}
		cfg.Telegram.AppID = id
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate rejects settings that would fail later in a confusing place, such as
// a segment size Telegram will refuse halfway through a multi-gigabyte upload.
func (c *Config) Validate() error {
	switch {
	case c.Storage.SegmentSize <= 0:
		return fmt.Errorf("segment size must be positive, got %d", c.Storage.SegmentSize)
	case c.Storage.SegmentSize > TelegramFileLimit:
		return fmt.Errorf(
			"segment size %d exceeds the %d byte ceiling of one Telegram object (%d parts of %d bytes)",
			c.Storage.SegmentSize, TelegramFileLimit, MaxUploadParts, UploadPartSize)
	case c.Storage.SegmentSize%UploadPartSize != 0:
		// A segment that is a whole number of upload parts keeps the last
		// part full, which avoids an undersized final part on every segment.
		return fmt.Errorf("segment size %d must be a multiple of the %d byte upload part size",
			c.Storage.SegmentSize, UploadPartSize)
	case c.Storage.SegmentConcurrency < 1:
		return fmt.Errorf("segment concurrency must be at least 1, got %d", c.Storage.SegmentConcurrency)
	case c.Telegram.PoolSize < 1:
		return fmt.Errorf("telegram pool size must be at least 1, got %d", c.Telegram.PoolSize)
	case c.Telegram.UploadThreads < 1:
		return fmt.Errorf("upload threads must be at least 1, got %d", c.Telegram.UploadThreads)
	case c.Stream.Concurrency < 1:
		return fmt.Errorf("stream concurrency must be at least 1, got %d", c.Stream.Concurrency)
	case c.Stream.Buffers < 1:
		return fmt.Errorf("stream buffers must be at least 1, got %d", c.Stream.Buffers)
	case c.Auth.BootstrapPassword != "" && len(c.Auth.BootstrapPassword) < 8:
		return fmt.Errorf("TDRIVE_ADMIN_PASSWORD must be at least 8 characters")
	case c.Auth.BootstrapUser != "" && c.Auth.BootstrapPassword == "":
		return fmt.Errorf("TDRIVE_ADMIN_USER was set without TDRIVE_ADMIN_PASSWORD")
	}
	return nil
}

// SegmentCount is how many Telegram objects a logical file of the given size
// occupies. A zero-byte file still occupies one, so that empty files exist.
func (c *Config) SegmentCount(size int64) int {
	if size <= 0 {
		return 1
	}
	n := size / c.Storage.SegmentSize
	if size%c.Storage.SegmentSize != 0 {
		n++
	}
	return int(n)
}

// defaultDataDir picks where to keep state when TDRIVE_DATA_DIR is unset.
//
// /data is the container convention and is what the Dockerfile sets
// explicitly, so this probe never runs there. It exists for the other case:
// running the binary directly, where /data is usually absent or owned by root
// and a ./data beside the working directory is what someone actually wants.
func defaultDataDir() string {
	const containerPath = "/data"

	if info, err := os.Stat(containerPath); err == nil && info.IsDir() {
		// Existing is not enough; it has to be writable by this user.
		probe, err := os.CreateTemp(containerPath, ".writable-")
		if err == nil {
			probe.Close()
			os.Remove(probe.Name())
			return containerPath
		}
	}
	return "data"
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envDur(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// envSize accepts a plain byte count or a human suffix, because writing
// TDRIVE_SEGMENT_SIZE=1900MiB is a lot easier to get right than 1992294400.
func envSize(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := ParseSize(v)
	if err != nil {
		return def
	}
	return n
}

// ParseSize converts "1900MiB", "2GB" or "1992294400" into bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}

	unit := int64(1)
	upper := strings.ToUpper(s)
	for _, suf := range []struct {
		name string
		mul  int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	} {
		if strings.HasSuffix(upper, suf.name) {
			unit = suf.mul
			s = strings.TrimSpace(s[:len(s)-len(suf.name)])
			break
		}
	}

	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q is negative", s)
	}
	return int64(n * float64(unit)), nil
}
