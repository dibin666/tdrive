package drive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/reader"
	"github.com/dibin/tdrive/internal/tagcodec"
)

// These tests run the whole segmentation path — split on upload, stitch on
// read — against an in-memory Telegram. This is the part of the program that
// cannot be exercised in CI against the real service and where a silent
// off-by-one would corrupt files rather than throw an error.

type harness struct {
	svc *Service
	db  *database.DB
	tg  *fakeTelegram
	cfg *config.Config
}

// newHarness builds a drive with a deliberately tiny segment size, so a file of
// a few kilobytes exercises the same multi-segment paths a 12 GB file would.
func newHarness(t *testing.T, segmentSize int64) *harness {
	t.Helper()
	return newHarnessN(t, segmentSize, 1)
}

// newHarnessN builds the same drive over a cluster of the given size, for the
// tests that care about how work is spread across Telegram accounts.
func newHarnessN(t *testing.T, segmentSize int64, accounts int) *harness {
	t.Helper()
	ctx := context.Background()

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "drive.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	channel, err := db.UpsertChannel(ctx, -1001234567890, 42, "test drive")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := db.SetDefaultChannel(ctx, channel.ID); err != nil {
		t.Fatalf("set default channel: %v", err)
	}

	cfg := &config.Config{}
	cfg.Storage.SegmentSize = segmentSize
	cfg.Storage.SegmentConcurrency = 2
	cfg.Storage.SpoolDir = t.TempDir()
	cfg.Stream.Concurrency = 4
	cfg.Stream.Buffers = 4
	cfg.Stream.LocationTTL = 0 // never trust the cache, so every read re-resolves

	tg := newFakeTelegramN(accounts)
	return &harness{
		svc: New(cfg, db, tg, zap.NewNop()),
		db:  db,
		tg:  tg,
		cfg: cfg,
	}
}

// open reads a file the way a real caller does: through an account. Which
// account is the scheduler's business, so tests that do not care about
// placement go through here.
func (h *harness) open(t *testing.T, file database.File) *reader.File {
	t.Helper()
	account, err := h.svc.ReadAccount(context.Background(), file.ID)
	if err != nil {
		t.Fatalf("pick a reading account for %q: %v", file.Name, err)
	}
	r, err := h.svc.OpenFile(context.Background(), file, account)
	if err != nil {
		t.Fatalf("open %q: %v", file.Name, err)
	}
	return r
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// store uploads a file through the sequential stream path and returns its row.
func (h *harness) store(t *testing.T, dir, name string, data []byte) database.File {
	t.Helper()
	file, err := h.svc.UploadStream(context.Background(), UploadRequest{
		DirPath: dir,
		Name:    name,
		Size:    int64(len(data)),
		UserID:  "",
	}, bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("upload %q: %v", name, err)
	}
	return file
}

func (h *harness) readAll(t *testing.T, file database.File) []byte {
	t.Helper()
	r := h.open(t, file)
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %q: %v", file.Name, err)
	}
	return got
}

// The headline requirement: a file too large for one Telegram object is stored
// as several and read back as one.
func TestSegmentedRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		size    int
		segSize int64
		wantSeg int
	}{
		{"fits in one segment", 3000, 4096, 1},
		{"exactly one segment", 4096, 4096, 1},
		{"one byte over", 4097, 4096, 2},
		{"several segments with a short last", 20_000, 4096, 5},
		{"exact multiple", 16_384, 4096, 4},
		{"single byte", 1, 4096, 1},
		{"larger than a read chunk", 3 << 20, 1 << 20, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.segSize)
			data := randomBytes(tc.size, int64(tc.size))

			file := h.store(t, "/", "payload.bin", data)
			if file.SegmentCount != tc.wantSeg {
				t.Errorf("stored in %d segments, want %d", file.SegmentCount, tc.wantSeg)
			}
			if file.Size != int64(tc.size) {
				t.Errorf("recorded size %d, want %d", file.Size, tc.size)
			}

			segs, err := h.db.Segments(context.Background(), file.ID)
			if err != nil {
				t.Fatalf("read segments: %v", err)
			}
			if len(segs) != tc.wantSeg {
				t.Fatalf("stored %d segment rows, want %d", len(segs), tc.wantSeg)
			}
			var total int64
			for _, s := range segs {
				total += s.Size
			}
			if total != int64(tc.size) {
				t.Errorf("segments hold %d bytes, want %d", total, tc.size)
			}

			if got := h.readAll(t, file); !bytes.Equal(got, data) {
				t.Fatalf("content mismatch: read %d bytes, want %d", len(got), len(data))
			}
		})
	}
}

// A read that starts or ends inside a segment, or spans several, must return
// exactly the requested bytes. This is what a video player scrubbing a split
// file does on every seek.
func TestRangeReadsAcrossSegmentBoundaries(t *testing.T) {
	const segSize = 4096
	h := newHarness(t, segSize)

	data := randomBytes(20_000, 99)
	file := h.store(t, "/", "movie.mkv", data)
	if file.SegmentCount < 4 {
		t.Fatalf("test needs a multi-segment file, got %d segments", file.SegmentCount)
	}

	// Every offset that matters: segment starts, segment ends, and the bytes
	// either side of each boundary.
	offsets := []int64{0, 1, segSize - 1, segSize, segSize + 1, 2*segSize - 1, 2 * segSize,
		3 * segSize, 4*segSize + 100, int64(len(data)) - 1}

	for _, start := range offsets {
		for _, length := range []int64{1, 100, segSize - 1, segSize, segSize + 1, 2 * segSize} {
			end := min(start+length, int64(len(data)))
			if start >= int64(len(data)) || end <= start {
				continue
			}

			t.Run(fmt.Sprintf("at=%d len=%d", start, end-start), func(t *testing.T) {
				r := h.open(t, file)
				defer r.Close()

				if _, err := r.Seek(start, io.SeekStart); err != nil {
					t.Fatalf("seek: %v", err)
				}
				got := make([]byte, end-start)
				if _, err := io.ReadFull(r, got); err != nil {
					t.Fatalf("read: %v", err)
				}
				if want := data[start:end]; !bytes.Equal(got, want) {
					t.Errorf("bytes [%d,%d) do not match the original", start, end)
				}
			})
		}
	}
}

// The browser slices a file itself and sends segments out of order and in
// parallel. The result has to be identical to the sequential path.
func TestParallelSegmentUploadMatchesSequential(t *testing.T) {
	const segSize = 4096
	h := newHarness(t, segSize)
	ctx := context.Background()

	data := randomBytes(20_000, 7)
	job, file, err := h.svc.Begin(ctx, UploadRequest{
		DirPath: "/parallel", Name: "big.bin", Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Deliberately not in index order.
	segments := map[int]io.Reader{}
	for i := file.SegmentCount; i >= 1; i-- {
		start := int64(i-1) * segSize
		end := min(start+segSize, int64(len(data)))
		segments[i] = bytes.NewReader(data[start:end])
	}

	if err := h.svc.PutSegmentsParallel(ctx, job, segments, nil); err != nil {
		t.Fatalf("parallel upload: %v", err)
	}
	done, err := h.svc.Complete(ctx, job.ID)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	if got := h.readAll(t, done); !bytes.Equal(got, data) {
		t.Fatal("content stored out of order did not read back correctly")
	}
}

// A plugin writes through the host stream, so it bypasses the HTTP handler
// that normally marks an upload as running. PutSegment must make the same
// transition before it starts consuming the stream; otherwise a one-segment
// plugin upload gets its StartedAt timestamp only when its final bytes land.
func TestPutSegmentRecordsStartBeforeTheStreamFinishes(t *testing.T) {
	const segmentSize = 4096
	h := newHarness(t, segmentSize)
	ctx := context.Background()
	data := randomBytes(segmentSize, 23)

	job, _, err := h.svc.Begin(ctx, UploadRequest{
		DirPath: "/", Name: "gated.bin", Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	reader := newStartGatedReader(data)
	defer reader.allow()
	uploadDone := make(chan error, 1)
	go func() {
		uploadDone <- h.svc.PutSegment(ctx, job, 1, reader, int64(len(data)), nil)
	}()

	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("the segment uploader did not start reading the source")
	}

	running, err := h.db.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("read running job: %v", err)
	}
	if running.Status != database.JobRunning {
		t.Fatalf("job status while the stream is blocked = %q, want running", running.Status)
	}
	if running.StartedAt.IsZero() {
		t.Fatal("job start time was not recorded before the stream finished")
	}
	if !running.FinishedAt.IsZero() {
		t.Fatal("job was marked finished before the stream finished")
	}

	reader.allow()
	select {
	case err := <-uploadDone:
		if err != nil {
			t.Fatalf("put segment: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the segment uploader did not finish after the source was released")
	}
}

type startGatedReader struct {
	source      *bytes.Reader
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newStartGatedReader(data []byte) *startGatedReader {
	return &startGatedReader{
		source:  bytes.NewReader(data),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (reader *startGatedReader) Read(buffer []byte) (int, error) {
	reader.startOnce.Do(func() { close(reader.started) })
	<-reader.release
	return reader.source.Read(buffer)
}

func (reader *startGatedReader) allow() {
	reader.releaseOnce.Do(func() { close(reader.release) })
}

// An interrupted upload must resume by re-sending only the segments that never
// landed, not the whole file.
func TestResumeSendsOnlyMissingSegments(t *testing.T) {
	const segSize = 4096
	h := newHarness(t, segSize)
	ctx := context.Background()

	data := randomBytes(20_000, 11)
	job, file, err := h.svc.Begin(ctx, UploadRequest{
		DirPath: "/", Name: "interrupted.bin", Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Send segments 1 and 3, then "crash".
	for _, idx := range []int{1, 3} {
		start := int64(idx-1) * segSize
		end := min(start+segSize, int64(len(data)))
		if err := h.svc.PutSegment(ctx, job, idx, bytes.NewReader(data[start:end]),
			end-start, nil); err != nil {
			t.Fatalf("put segment %d: %v", idx, err)
		}
	}

	resumed, err := h.db.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	pending := resumed.PendingSegments()
	want := []int{2, 4, 5}
	if len(pending) != len(want) {
		t.Fatalf("pending = %v, want %v", pending, want)
	}
	for i := range want {
		if pending[i] != want[i] {
			t.Fatalf("pending = %v, want %v", pending, want)
		}
	}

	before := h.tg.uploads.Load()
	for _, idx := range pending {
		start := int64(idx-1) * segSize
		end := min(start+segSize, int64(len(data)))
		if err := h.svc.PutSegment(ctx, resumed, idx, bytes.NewReader(data[start:end]),
			end-start, nil); err != nil {
			t.Fatalf("resume segment %d: %v", idx, err)
		}
	}
	if sent := h.tg.uploads.Load() - before; sent != int64(len(want)) {
		t.Errorf("resume sent %d segments, want %d", sent, len(want))
	}

	final, err := h.svc.Complete(ctx, job.ID)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := h.readAll(t, final); !bytes.Equal(got, data) {
		t.Fatal("resumed file did not read back correctly")
	}
	_ = file
}

// An expired file reference is the single most common recoverable failure in
// production. Reads must repair it rather than surface it.
func TestReadRecoversFromExpiredFileReference(t *testing.T) {
	h := newHarness(t, 4096)
	data := randomBytes(12_000, 3)
	file := h.store(t, "/", "aged.bin", data)

	// An hour passes: every reference handed out at upload time goes stale.
	h.tg.expireRefs()

	if got := h.readAll(t, file); !bytes.Equal(got, data) {
		t.Fatal("read did not recover from an expired file reference")
	}
}

// A document that has migrated to another datacenter must be found there and
// the location remembered.
func TestReadFollowsDatacenterMigration(t *testing.T) {
	h := newHarness(t, 4096)
	data := randomBytes(12_000, 5)
	file := h.store(t, "/", "moved.bin", data)

	h.tg.migrateTo = 4

	if got := h.readAll(t, file); !bytes.Equal(got, data) {
		t.Fatal("read did not follow the datacenter migration")
	}

	segs, err := h.db.Segments(context.Background(), file.ID)
	if err != nil {
		t.Fatalf("read segments: %v", err)
	}
	for _, seg := range segs {
		if seg.DCID != 4 {
			t.Errorf("segment %d still records DC%d; the migration was not persisted",
				seg.Index, seg.DCID)
		}
	}
}

// Zero-byte files have to exist. Telegram will not store a zero-part document,
// so they are recorded as a caption only.
func TestEmptyFile(t *testing.T) {
	h := newHarness(t, 4096)
	file := h.store(t, "/", "empty.txt", nil)

	if file.SegmentCount != 1 {
		t.Errorf("empty file has %d segments, want 1", file.SegmentCount)
	}
	if got := h.readAll(t, file); len(got) != 0 {
		t.Errorf("read %d bytes from an empty file", len(got))
	}

	entry, err := h.svc.Stat(context.Background(), "/empty.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if entry.Size != 0 {
		t.Errorf("stat reports %d bytes", entry.Size)
	}
}

// Every caption written must decode back into the record it describes, because
// that is the only thing a rebuild has to work from.
func TestCaptionsAreSelfDescribing(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	if _, err := h.svc.Mkdir(ctx, "/电影/2024"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data := randomBytes(12_000, 13)
	file := h.store(t, "/电影/2024", "影片 4K.mkv", data)

	var (
		dirRecords  int
		fileRecords int
		segIndices  = map[int]bool{}
	)
	for _, caption := range h.tg.captions() {
		rec, err := tagcodec.Decode(caption)
		if err != nil {
			t.Errorf("caption did not decode: %v\n%s", err, caption)
			continue
		}
		switch rec.Kind {
		case tagcodec.KindDir:
			dirRecords++
		case tagcodec.KindFile:
			fileRecords++
			if rec.ID != file.ID {
				t.Errorf("file record has id %q, want %q", rec.ID, file.ID)
			}
			if rec.Name != "影片 4K.mkv" {
				t.Errorf("file record has name %q", rec.Name)
			}
			if rec.TotalSize != int64(len(data)) {
				t.Errorf("file record has size %d, want %d", rec.TotalSize, len(data))
			}
			if rec.SegCount != file.SegmentCount {
				t.Errorf("file record claims %d segments, want %d", rec.SegCount, file.SegmentCount)
			}
			segIndices[rec.SegIndex] = true
			// The folder tags are what make "#电影" work as a filter inside
			// a Telegram client.
			if !strings.Contains(caption, "#电影") {
				t.Errorf("caption is missing the folder tag:\n%s", caption)
			}
		}
	}

	if dirRecords != 2 {
		t.Errorf("wrote %d directory records, want 2 (电影 and 2024)", dirRecords)
	}
	if fileRecords != file.SegmentCount {
		t.Errorf("wrote %d file records, want one per segment (%d)", fileRecords, file.SegmentCount)
	}
	for i := 1; i <= file.SegmentCount; i++ {
		if !segIndices[i] {
			t.Errorf("no caption claims segment %d", i)
		}
	}
}

// A rename has to reach the captions too, or a rebuild would resurrect the old
// name.
func TestRenameRewritesEverySegmentCaption(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	data := randomBytes(12_000, 17)
	file := h.store(t, "/docs", "old.bin", data)

	if _, err := h.svc.Rename(ctx, "/docs/old.bin", "new.bin"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	renamed := 0
	for _, caption := range h.tg.captions() {
		rec, err := tagcodec.Decode(caption)
		if err != nil || rec.Kind != tagcodec.KindFile {
			continue
		}
		if rec.Name != "new.bin" {
			t.Errorf("segment %d still records the old name %q", rec.SegIndex, rec.Name)
		}
		renamed++
	}
	if renamed != file.SegmentCount {
		t.Errorf("updated %d captions, want %d", renamed, file.SegmentCount)
	}

	// The bytes must still be readable: editing a caption rotates the file
	// reference, and a cached stale one would break every read.
	updated, err := h.db.FileByID(ctx, file.ID)
	if err != nil {
		t.Fatalf("reload file: %v", err)
	}
	if got := h.readAll(t, updated); !bytes.Equal(got, data) {
		t.Fatal("file became unreadable after a rename rotated its file references")
	}
}

// Deleting must take the Telegram messages with it, or the channel accumulates
// documents nothing can reach.
func TestDeleteRemovesEveryMessage(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	h.store(t, "/trash/inner", "a.bin", randomBytes(12_000, 19))
	h.store(t, "/trash/inner", "b.bin", randomBytes(5_000, 23))
	h.store(t, "/keep", "c.bin", randomBytes(1_000, 29))

	before := h.tg.messageCount()
	if before == 0 {
		t.Fatal("nothing was stored")
	}

	if err := h.svc.Delete(ctx, "/trash"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// What should remain: the /keep directory record and c.bin's one segment.
	if after := h.tg.messageCount(); after != 2 {
		t.Errorf("%d messages left in the channel, want 2", after)
	}
	if _, err := h.svc.Stat(ctx, "/trash"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted directory still resolves: %v", err)
	}
	if _, err := h.svc.Stat(ctx, "/keep/c.bin"); err != nil {
		t.Errorf("unrelated file was removed: %v", err)
	}
}

// A PUT of unknown length is the WebDAV chunked-encoding case. It has to work,
// and the result has to be identical to a sized upload.
func TestUnsizedUpload(t *testing.T) {
	h := newHarness(t, 4096)
	data := randomBytes(15_000, 31)

	file, err := h.svc.UploadUnsized(context.Background(), UploadRequest{
		DirPath: "/", Name: "chunked.bin", Size: -1,
	}, bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("unsized upload: %v", err)
	}

	if file.Size != int64(len(data)) {
		t.Errorf("recorded size %d, want %d", file.Size, len(data))
	}
	if got := h.readAll(t, file); !bytes.Equal(got, data) {
		t.Fatal("unsized upload did not round-trip")
	}
}

func TestOverwriteReplacesTheOldFile(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	first := randomBytes(10_000, 37)
	h.store(t, "/", "same.bin", first)

	// Without Overwrite the second upload must be refused.
	_, err := h.svc.UploadStream(ctx, UploadRequest{
		DirPath: "/", Name: "same.bin", Size: 10,
	}, bytes.NewReader(make([]byte, 10)), nil)
	if !errors.Is(err, ErrExists) {
		t.Errorf("duplicate upload returned %v, want ErrExists", err)
	}

	second := randomBytes(3_000, 41)
	file, err := h.svc.UploadStream(ctx, UploadRequest{
		DirPath: "/", Name: "same.bin", Size: int64(len(second)), Overwrite: true,
	}, bytes.NewReader(second), nil)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got := h.readAll(t, file); !bytes.Equal(got, second) {
		t.Fatal("overwrite left the old content in place")
	}

	entries, err := h.svc.List(ctx, "/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.Name == "same.bin" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("listing shows %d copies of same.bin, want 1", count)
	}
}

func TestOverwriteQuotaCountsNetSize(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()
	user, err := h.db.CreateUser(ctx, "quota-user", "hash", database.RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	quota := int64(10)
	if err := h.db.UpdateUserProfile(ctx, user.ID, database.UserProfile{QuotaBytes: &quota}); err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}

	old := bytes.Repeat([]byte("o"), 8)
	if _, err := h.svc.UploadStream(ctx, UploadRequest{
		DirPath: "/", Name: "quota.bin", Size: int64(len(old)), UserID: user.ID,
	}, bytes.NewReader(old), nil); err != nil {
		t.Fatalf("initial upload: %v", err)
	}

	newData := bytes.Repeat([]byte("n"), 8)
	if _, err := h.svc.UploadStream(ctx, UploadRequest{
		DirPath: "/", Name: "quota.bin", Size: int64(len(newData)), UserID: user.ID, Overwrite: true,
	}, bytes.NewReader(newData), nil); err != nil {
		t.Fatalf("same-sized overwrite should fit quota: %v", err)
	}
}

// Listings must present one entry per logical file, whatever its segment count.
func TestListingHidesSegmentation(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	big := randomBytes(40_000, 43) // ten segments
	file := h.store(t, "/", "huge.bin", big)
	if file.SegmentCount < 5 {
		t.Fatalf("test needs a many-segment file, got %d", file.SegmentCount)
	}

	entries, err := h.svc.List(ctx, "/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("listing has %d entries, want exactly 1", len(entries))
	}
	if entries[0].Size != int64(len(big)) {
		t.Errorf("listing reports %d bytes, want the logical total %d", entries[0].Size, len(big))
	}
	if entries[0].SegmentCount != file.SegmentCount {
		t.Errorf("segment count %d not surfaced for the details panel", entries[0].SegmentCount)
	}
}

// Move must not let a directory become its own ancestor.
func TestMoveRejectsCycles(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	if _, err := h.svc.Mkdir(ctx, "/a/b/c"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := h.svc.Move(ctx, "/a", "/a/b"); !errors.Is(err, ErrLoop) {
		t.Errorf("moving a directory into its own child returned %v, want ErrLoop", err)
	}
	if _, err := h.svc.Move(ctx, "/a", "/a"); !errors.Is(err, ErrLoop) {
		t.Errorf("moving a directory into itself returned %v, want ErrLoop", err)
	}
}

func TestMoveFileBetweenDirectories(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	data := randomBytes(9_000, 47)
	h.store(t, "/from", "file.bin", data)

	if _, err := h.svc.Move(ctx, "/from/file.bin", "/to/nested"); err != nil {
		t.Fatalf("move: %v", err)
	}

	moved, err := h.svc.Stat(ctx, "/to/nested/file.bin")
	if err != nil {
		t.Fatalf("stat after move: %v", err)
	}
	if moved.Size != int64(len(data)) {
		t.Errorf("size changed during the move")
	}
	if _, err := h.svc.Stat(ctx, "/from/file.bin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("file still resolves at its old path")
	}

	file, err := h.db.FileByID(ctx, moved.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := h.readAll(t, file); !bytes.Equal(got, data) {
		t.Fatal("file became unreadable after the move")
	}
}

// A short read must not pull whole segments. This is what keeps a media
// player's initial probe from downloading megabytes of a 12 GB file.
func TestSmallReadTouchesOnlyOneSegment(t *testing.T) {
	h := newHarness(t, 1<<20)
	data := randomBytes(4<<20, 53)
	file := h.store(t, "/", "movie.mp4", data)

	before := h.tg.reads.Load()

	r := h.open(t, file)
	defer r.Close()

	head := make([]byte, 4096)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(head, data[:4096]) {
		t.Error("header bytes do not match")
	}

	// A single aligned chunk is enough to serve 4 KiB.
	if reads := h.tg.reads.Load() - before; reads > 2 {
		t.Errorf("serving 4 KiB took %d reads from Telegram", reads)
	}
}

func TestNameValidationRejectsHostileInput(t *testing.T) {
	h := newHarness(t, 4096)
	ctx := context.Background()

	for _, name := range []string{
		"", ".", "..", "a/b", "a\x00b", "a\nb",
		strings.Repeat("x", tagcodec.MaxNameBytes+1),
	} {
		_, err := h.svc.UploadStream(ctx, UploadRequest{
			DirPath: "/", Name: name, Size: 3,
		}, bytes.NewReader([]byte("abc")), nil)
		if err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}
