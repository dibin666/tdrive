// Package config loads tdrive's deployment configuration from environment
// variables and holds the runtime settings that the WebUI can change.
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
	// DefaultUploadPartSize is the default MTProto part size for
	// upload.saveBigFilePart. It is configurable at runtime, but Telegram only
	// accepts sizes that divide this maximum.
	DefaultUploadPartSize int64 = 512 * 1024
	// UploadPartSize is kept as a compatibility alias for code and tests that
	// refer to the historical fixed default.
	UploadPartSize = DefaultUploadPartSize
	// MaxUploadParts is the MTProto limit on file_total_parts.
	MaxUploadParts = 4000
	// TelegramFileLimit is the absolute largest single object
	// upload.saveBigFilePart can express with Telegram's maximum 512 KiB part:
	// 4000 parts is 2000 MiB. A smaller configured part size lowers the limit for
	// each new segment accordingly.
	TelegramFileLimit int64 = MaxUploadParts * DefaultUploadPartSize
	// DefaultRateLimit is the delay between Telegram RPC requests by default.
	DefaultRateLimit = 100 * time.Millisecond
	// DefaultUploadConcurrency is the number of whole-file upload tasks allowed
	// to use Telegram at once. Parts inside one task are governed separately by
	// UploadThreads and the browser's segment pipeline.
	DefaultUploadConcurrency = 2
	// DefaultDownloadConcurrency is the number of whole-file download tasks
	// allowed to read Telegram at once. Chunk prefetch inside one task is
	// governed separately by Stream.Concurrency.
	DefaultDownloadConcurrency = 2

	// DefaultSegmentSize is 1900 MiB, exactly 3800 default-size upload parts. The
	// 100 MiB of headroom under the default 2000 MiB object ceiling keeps a
	// segment from landing on the boundary where Telegram starts rejecting it.
	DefaultSegmentSize int64 = 1900 * 1024 * 1024

	// DownloadChunkSize is the largest upload.getFile response Telegram will
	// return, and the unit the parallel reader prefetches in.
	DownloadChunkSize int64 = 1024 * 1024

	// DefaultCacheLimit bounds the disk held by staged downloads. Staging is
	// what makes a many-segment file safe to pull with a parallel downloader,
	// but it is the one feature here that can fill a VPS, so it is capped by
	// default rather than opt-in.
	DefaultCacheLimit int64 = 20 << 30
	// DefaultCacheTTL is how long a staged copy survives once it is ready.
	DefaultCacheTTL = 24 * time.Hour
	// DefaultPluginBinaryLimit bounds a downloaded plugin executable. A Go
	// binary with an embedded UI is tens of megabytes; this leaves generous
	// room while still refusing to stream an unbounded body to disk.
	DefaultPluginBinaryLimit int64 = 256 << 20
	// DefaultMaxDownloadConns is how many parallel range requests one logical
	// download may hold. Parallelism is the point of a reusable link; without
	// a cap, one client could open enough sockets to starve everyone else.
	DefaultMaxDownloadConns = 8
	// DefaultDownloadGrace is how long a download session keeps its task slot
	// after its last request finishes. A multi-threaded downloader routinely
	// has a moment with nothing in flight between ranges, and losing the slot
	// there would send it to the back of the queue mid-file.
	DefaultDownloadGrace = 15 * time.Second
	// DefaultShareTTL is how long a share link lasts unless the caller says
	// otherwise. Zero would mean "forever", which is not a good default for a
	// bearer capability.
	DefaultShareTTL = 7 * 24 * time.Hour
)

// Config is the fully resolved configuration. Load validates it before
// returning, so nothing downstream needs to re-check these values.
type Config struct {
	Server   Server
	Auth     Auth
	Telegram Telegram
	Storage  Storage
	Stream   Stream
	Transfer Transfer
	WebDAV   WebDAV
	Local    Local
	Plugins  Plugins
	LogLevel string
	Runtime  *RuntimeConfig
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
	// UploadPartSize is the size of each Telegram upload part. It is separate
	// from Storage.SegmentSize: the latter is the logical file split shown by
	// the drive, while this is the MTProto request payload size.
	UploadPartSize int64

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
	// CacheDir is the default location for staged downloads. The effective
	// value lives in RuntimeSettings so an administrator can move it onto a
	// bigger volume without restarting.
	CacheDir string
	// CacheLimit bounds the disk staged downloads may hold; 0 turns staging
	// off, which is the right answer on a host with no spare space.
	CacheLimit int64
	// CacheTTL is how long a staged copy survives after it is ready.
	CacheTTL time.Duration
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

// Transfer holds the task-level concurrency limits. These limits are distinct
// from the per-task Telegram part/chunk concurrency above: one file consumes
// one task slot for its entire upload or download.
type Transfer struct {
	UploadConcurrency   int
	DownloadConcurrency int
	// MaxDownloadConns caps the parallel range requests belonging to one
	// logical download.
	MaxDownloadConns int
	// DownloadGrace is how long a download session holds its task slot after
	// its last in-flight request finishes.
	DownloadGrace time.Duration
	// ShareTTL is the default lifetime of a share link.
	ShareTTL time.Duration
}

type WebDAV struct {
	Enabled bool
	Prefix  string
}

// Local describes the optional read-only directory exposed as a source for
// server-side uploads. Root is the legacy environment fallback; the effective
// value lives in RuntimeSettings so an administrator can change it in the
// WebUI without restarting the process.
type Local struct {
	Root string
}

// Plugins contains deployment-level plugin settings. No plugin download
// happens on the startup path: tdrive only reaches the network when an
// administrator explicitly inspects or installs a plugin.
type Plugins struct {
	// Dir stores installed plugin binaries and staging files.
	Dir string
	// StoreURL is an optional HTTPS JSON index. An empty value disables the
	// network store without disabling installation from a manifest URL.
	StoreURL string
	// MaxBinaryBytes caps a downloaded plugin executable. Zero selects
	// DefaultPluginBinaryLimit, so a hand-built configuration still gets a
	// bound rather than refusing every download.
	MaxBinaryBytes int64
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

	localRoot, err := NormalizeLocalRoot(os.Getenv("TDRIVE_LOCAL_DIR"))
	if err != nil {
		return nil, err
	}
	pluginDir := envStr("TDRIVE_PLUGIN_DIR", filepath.Join(dataDir, "plugins"))
	pluginDir, err = filepath.Abs(pluginDir)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin dir %q: %w", pluginDir, err)
	}

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
			AppHash:        envStr("TDRIVE_TG_APP_HASH", ""),
			PoolSize:       int64(envInt("TDRIVE_TG_POOL_SIZE", 8)),
			UploadThreads:  envInt("TDRIVE_UPLOAD_THREADS", 8),
			UploadPartSize: envSize("TDRIVE_TG_UPLOAD_PART_SIZE", DefaultUploadPartSize),
			RateLimit:      envDur("TDRIVE_TG_RATE_LIMIT", DefaultRateLimit),
			RateBurst:      envInt("TDRIVE_TG_RATE_BURST", 5),
			SessionFile:    filepath.Join(dataDir, "session.json"),
		},
		Storage: Storage{
			SegmentSize:        envSize("TDRIVE_SEGMENT_SIZE", DefaultSegmentSize),
			SegmentConcurrency: envInt("TDRIVE_SEGMENT_CONCURRENCY", 2),
			SpoolDir:           filepath.Join(dataDir, "spool"),
			DatabaseFile:       filepath.Join(dataDir, "tdrive.db"),
			CacheDir:           envStr("TDRIVE_CACHE_DIR", filepath.Join(dataDir, "cache")),
			CacheLimit:         envSize("TDRIVE_CACHE_LIMIT", DefaultCacheLimit),
			CacheTTL:           envDur("TDRIVE_CACHE_TTL", DefaultCacheTTL),
		},
		Stream: Stream{
			Concurrency:  envInt("TDRIVE_STREAM_CONCURRENCY", 6),
			Buffers:      envInt("TDRIVE_STREAM_BUFFERS", 8),
			ChunkTimeout: envDur("TDRIVE_CHUNK_TIMEOUT", 30*time.Second),
			LocationTTL:  envDur("TDRIVE_LOCATION_TTL", 30*time.Minute),
		},
		Transfer: Transfer{
			UploadConcurrency:   envInt("TDRIVE_UPLOAD_CONCURRENCY", DefaultUploadConcurrency),
			DownloadConcurrency: envInt("TDRIVE_DOWNLOAD_CONCURRENCY", DefaultDownloadConcurrency),
			MaxDownloadConns:    envInt("TDRIVE_MAX_DOWNLOAD_CONNS", DefaultMaxDownloadConns),
			DownloadGrace:       envDur("TDRIVE_DOWNLOAD_GRACE", DefaultDownloadGrace),
			ShareTTL:            envDur("TDRIVE_SHARE_TTL", DefaultShareTTL),
		},
		WebDAV: WebDAV{
			Enabled: envBool("TDRIVE_WEBDAV_ENABLED", true),
			Prefix:  "/dav",
		},
		Local: Local{
			Root: localRoot,
		},
		Plugins: Plugins{
			Dir:            pluginDir,
			StoreURL:       strings.TrimSuffix(envStrAllowEmpty("TDRIVE_PLUGIN_STORE_URL", "https://raw.githubusercontent.com/dibin666/tdrive/main/plugins/index.json"), "/"),
			MaxBinaryBytes: envSize("TDRIVE_PLUGIN_MAX_BINARY_BYTES", DefaultPluginBinaryLimit),
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
	cfg.Runtime = NewRuntimeConfig(cfg.RuntimeSettings())

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NormalizeLocalRoot converts a WebUI or environment-provided local source
// directory into a stable absolute path. An empty value disables VPS-local
// uploads. The directory is intentionally not required to exist here because a
// Docker bind mount may be added after the process starts.
func NormalizeLocalRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve local directory %q: %w", root, err)
	}
	return absolute, nil
}

// Validate rejects settings that would fail later in a confusing place, such as
// a segment size Telegram will refuse halfway through a multi-gigabyte upload.
func (c *Config) Validate() error {
	if err := c.RuntimeSettings().Validate(); err != nil {
		return err
	}
	switch {
	case c.Storage.SegmentConcurrency < 1:
		return fmt.Errorf("segment concurrency must be at least 1, got %d", c.Storage.SegmentConcurrency)
	case c.Stream.Concurrency < 1:
		return fmt.Errorf("stream concurrency must be at least 1, got %d", c.Stream.Concurrency)
	case c.Stream.Buffers < 1:
		return fmt.Errorf("stream buffers must be at least 1, got %d", c.Stream.Buffers)
	case c.Auth.BootstrapPassword != "" && len(c.Auth.BootstrapPassword) < 8:
		return fmt.Errorf("TDRIVE_ADMIN_PASSWORD must be at least 8 characters")
	case c.Auth.BootstrapUser != "" && c.Auth.BootstrapPassword == "":
		return fmt.Errorf("TDRIVE_ADMIN_USER was set without TDRIVE_ADMIN_PASSWORD")
	case c.Plugins.MaxBinaryBytes < 0:
		return fmt.Errorf("plugin binary size limit must not be negative, got %d", c.Plugins.MaxBinaryBytes)
	case c.Plugins.MaxBinaryBytes > 4<<30:
		return fmt.Errorf("plugin binary size limit must not exceed 4GiB, got %d", c.Plugins.MaxBinaryBytes)
	}
	return nil
}

// SegmentCount is how many Telegram objects a logical file of the given size
// occupies. A zero-byte file still occupies one, so that empty files exist.
func (c *Config) SegmentCount(size int64) int {
	if size <= 0 {
		return 1
	}
	segmentSize := c.RuntimeSettings().SegmentSize
	n := size / segmentSize
	if size%segmentSize != 0 {
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

// envStrAllowEmpty distinguishes an unset variable from an explicit empty
// value. Plugin deployments use that distinction to disable the default store
// or the local builder while still retaining useful defaults when no variable
// was provided.
func envStrAllowEmpty(key, def string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
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
