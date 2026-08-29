package drive

import (
	"strings"
	"testing"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/tagcodec"
)

func TestCleanPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", Root},
		{"/", Root},
		{"//", Root},
		{"/Movies", "/Movies"},
		{"Movies", "/Movies"},
		{"/Movies/", "/Movies"},
		{"/Movies//2024", "/Movies/2024"},
		{"/Movies/./2024", "/Movies/2024"},
		{"/Movies/2024/..", "/Movies"},
		{"/../etc/passwd", "/etc/passwd"},
		{"/电影/2024年/4K", "/电影/2024年/4K"},
		{"/a b/c.d", "/a b/c.d"},
	}
	for _, tc := range cases {
		got, err := CleanPath(tc.in)
		if err != nil {
			t.Errorf("CleanPath(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CleanPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Anything a client can send has to either normalise into the drive or be
// rejected. Silently resolving outside the root is the failure that matters.
func TestCleanPathCannotEscapeTheRoot(t *testing.T) {
	for _, in := range []string{
		"../../../etc/passwd",
		"/../../..",
		"/a/../../b",
		"/./../",
	} {
		got, err := CleanPath(in)
		if err != nil {
			continue // rejecting is also fine
		}
		if !strings.HasPrefix(got, "/") || strings.Contains(got, "..") {
			t.Errorf("CleanPath(%q) = %q, which escapes the root", in, got)
		}
	}
}

func TestCleanPathRejectsBadNames(t *testing.T) {
	for _, in := range []string{
		"/a\x00b",
		"/ok/\x01bad",
		"/tab\vhere",
		"/" + strings.Repeat("x", tagcodec.MaxNameBytes+1),
	} {
		if _, err := CleanPath(in); err == nil {
			t.Errorf("CleanPath(%q) was accepted", in)
		}
	}
}

func TestParentAndJoin(t *testing.T) {
	cases := []struct {
		path, dir, name string
	}{
		{"/a", Root, "a"},
		{"/a/b", "/a", "b"},
		{"/a/b/c", "/a/b", "c"},
		{Root, Root, ""},
		{"/电影/影片.mkv", "/电影", "影片.mkv"},
	}
	for _, tc := range cases {
		dir, name := Parent(tc.path)
		if dir != tc.dir || name != tc.name {
			t.Errorf("Parent(%q) = (%q, %q), want (%q, %q)", tc.path, dir, name, tc.dir, tc.name)
		}
		if tc.name != "" {
			if got := Join(dir, name); got != tc.path {
				t.Errorf("Join(%q, %q) = %q, want %q", dir, name, got, tc.path)
			}
		}
	}
}

func TestIsDescendant(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/a", "/a/b", true},
		{"/a", "/a/b/c", true},
		{"/a", "/a", false},
		{"/a", "/ab", false},
		{"/a", "/b", false},
		{Root, "/a", true},
		{Root, Root, false},
	}
	for _, tc := range cases {
		if got := IsDescendant(tc.parent, tc.child); got != tc.want {
			t.Errorf("IsDescendant(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}

func TestAncestorNamesAreNearestFirst(t *testing.T) {
	got := AncestorNames("/电影/2024/4K")
	want := []string{"4K", "2024", "电影"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if n := len(AncestorNames(Root)); n != 0 {
		t.Errorf("root has %d ancestors, want 0", n)
	}
}

// The segment layout is what the reader's arithmetic assumes: every segment but
// the last is exactly SegmentSize. If these disagree, downloads corrupt.
func TestSegmentSizesTileTheFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.SegmentSize = 1000

	for _, total := range []int64{0, 1, 999, 1000, 1001, 2000, 2001, 12345} {
		count := cfg.SegmentCount(total)
		var sum int64
		for i := 1; i <= count; i++ {
			size := SegmentSize(total, 1000, i)
			if i < count && size != 1000 {
				t.Errorf("total %d: segment %d of %d is %d bytes, want a full 1000",
					total, i, count, size)
			}
			if size < 0 || size > 1000 {
				t.Errorf("total %d: segment %d has impossible size %d", total, i, size)
			}
			sum += size
		}
		if sum != total {
			t.Errorf("total %d: segments add up to %d across %d segments", total, sum, count)
		}
		if count < 1 {
			t.Errorf("total %d: got %d segments, every file needs at least one", total, count)
		}
	}
}

// A file at exactly the segment boundary must not gain an empty trailing
// segment, and one byte past it must gain a real one.
func TestSegmentCountAtBoundaries(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.SegmentSize = config.DefaultSegmentSize

	cases := []struct {
		size int64
		want int
	}{
		{0, 1},
		{1, 1},
		{config.DefaultSegmentSize - 1, 1},
		{config.DefaultSegmentSize, 1},
		{config.DefaultSegmentSize + 1, 2},
		{config.DefaultSegmentSize * 2, 2},
		{config.DefaultSegmentSize*2 + 1, 3},
		{12 << 30, 7}, // a 12 GiB file at the default split size
	}
	for _, tc := range cases {
		if got := cfg.SegmentCount(tc.size); got != tc.want {
			t.Errorf("SegmentCount(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
}

// The default segment size has to stay inside what one Telegram object can
// hold, and divide evenly into upload parts.
func TestDefaultSegmentSizeFitsTelegram(t *testing.T) {
	if config.DefaultSegmentSize > config.TelegramFileLimit {
		t.Fatalf("default segment size %d exceeds the %d byte object limit",
			config.DefaultSegmentSize, config.TelegramFileLimit)
	}
	if config.DefaultSegmentSize%config.UploadPartSize != 0 {
		t.Errorf("default segment size %d is not a whole number of %d byte parts",
			config.DefaultSegmentSize, config.UploadPartSize)
	}
	parts := config.DefaultSegmentSize / config.UploadPartSize
	if parts > config.MaxUploadParts {
		t.Errorf("default segment needs %d upload parts, the limit is %d",
			parts, config.MaxUploadParts)
	}
}

func TestConfigRejectsUnusableSegmentSizes(t *testing.T) {
	base := func() *config.Config {
		c := &config.Config{}
		c.Storage.SegmentSize = config.DefaultSegmentSize
		c.Storage.SegmentConcurrency = 2
		c.Telegram.PoolSize = 8
		c.Telegram.UploadThreads = 8
		c.Stream.Concurrency = 6
		c.Stream.Buffers = 8
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("the default configuration is invalid: %v", err)
	}

	over := base()
	over.Storage.SegmentSize = config.TelegramFileLimit + config.UploadPartSize
	if err := over.Validate(); err == nil {
		t.Error("a segment larger than one Telegram object was accepted")
	}

	ragged := base()
	ragged.Storage.SegmentSize = config.DefaultSegmentSize + 1
	if err := ragged.Validate(); err == nil {
		t.Error("a segment size that is not a multiple of the part size was accepted")
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"1KiB", 1024},
		{"1MiB", 1 << 20},
		{"1900MiB", config.DefaultSegmentSize},
		{"2GB", 2_000_000_000},
		{"1.5MiB", 1572864},
		{" 512K ", 512 << 10},
	}
	for _, tc := range cases {
		got, err := config.ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "abc", "-5MiB", "MiB"} {
		if _, err := config.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) was accepted", bad)
		}
	}
}

func TestGuessMIME(t *testing.T) {
	cases := []struct{ name, want string }{
		{"a.mkv", "video/x-matroska"},
		{"a.7z", "application/x-7z-compressed"},
		{"a.unknownext", "application/octet-stream"},
		{"noext", "application/octet-stream"},
	}
	for _, tc := range cases {
		if got := GuessMIME(tc.name); got != tc.want {
			t.Errorf("GuessMIME(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := GuessMIME("a.mp4"); !strings.HasPrefix(got, "video/") {
		t.Errorf("GuessMIME(\"a.mp4\") = %q, want a video type", got)
	}
}
