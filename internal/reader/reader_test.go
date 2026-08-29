package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDrive stores a logical file split into segments, and hands out Sources
// that behave the way Telegram does: byte-exact reads bounded to the segment.
type fakeDrive struct {
	data    []byte
	segSize int64

	calls   atomic.Int64
	bytes   atomic.Int64
	failSeg int   // 1-based; 0 disables
	failAt  int64 // offset within that segment that errors
	delay   time.Duration
}

func newFakeDrive(size, segSize int64) *fakeDrive {
	data := make([]byte, size)
	// A deterministic non-repeating pattern makes an off-by-one in the
	// stitching show up as a content mismatch rather than as identical bytes.
	rand.New(rand.NewSource(size)).Read(data)
	return &fakeDrive{data: data, segSize: segSize}
}

func (d *fakeDrive) segment(idx int) []byte {
	start := int64(idx-1) * d.segSize
	if start >= int64(len(d.data)) {
		return nil
	}
	end := min(start+d.segSize, int64(len(d.data)))
	return d.data[start:end]
}

func (d *fakeDrive) sourceFor(_ context.Context, idx int) (Source, error) {
	seg := d.segment(idx)
	if seg == nil {
		return nil, fmt.Errorf("no segment %d", idx)
	}
	return &fakeSource{drive: d, idx: idx, data: seg}, nil
}

type fakeSource struct {
	drive *fakeDrive
	idx   int
	data  []byte
}

func (s *fakeSource) Chunk(ctx context.Context, offset, limit int64) ([]byte, error) {
	s.drive.calls.Add(1)
	if s.drive.delay > 0 {
		select {
		case <-time.After(s.drive.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.drive.failSeg == s.idx && offset >= s.drive.failAt {
		return nil, errors.New("simulated telegram failure")
	}

	// Telegram never crosses a 1 MiB window, and neither may we ask it to.
	if offset/(1<<20) != (offset+limit-1)/(1<<20) {
		return nil, fmt.Errorf("read [%d,%d) crosses a 1 MiB boundary", offset, offset+limit)
	}
	if limit > 1<<20 {
		return nil, fmt.Errorf("limit %d exceeds 1 MiB", limit)
	}
	if offset >= int64(len(s.data)) {
		return nil, nil
	}

	end := min(offset+limit, int64(len(s.data)))
	out := make([]byte, end-offset)
	copy(out, s.data[offset:end])
	s.drive.bytes.Add(int64(len(out)))
	return out, nil
}

func TestPlanRanges(t *testing.T) {
	cases := []struct {
		name             string
		start, end, size int64
		want             []SegRange
	}{
		{
			name:  "entirely inside one segment",
			start: 10, end: 20, size: 100,
			want: []SegRange{{Index: 1, Start: 10, End: 20}},
		},
		{
			name:  "spans two segments",
			start: 90, end: 110, size: 100,
			want: []SegRange{{Index: 1, Start: 90, End: 99}, {Index: 2, Start: 0, End: 10}},
		},
		{
			name:  "spans three segments end to end",
			start: 0, end: 299, size: 100,
			want: []SegRange{
				{Index: 1, Start: 0, End: 99},
				{Index: 2, Start: 0, End: 99},
				{Index: 3, Start: 0, End: 99},
			},
		},
		{
			name:  "exactly on a boundary",
			start: 100, end: 100, size: 100,
			want: []SegRange{{Index: 2, Start: 0, End: 0}},
		},
		{
			name:  "last byte of a segment",
			start: 99, end: 99, size: 100,
			want: []SegRange{{Index: 1, Start: 99, End: 99}},
		},
		{
			name:  "single byte at offset zero",
			start: 0, end: 0, size: 100,
			want: []SegRange{{Index: 1, Start: 0, End: 0}},
		},
		{
			name:  "empty range",
			start: 50, end: 49, size: 100,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planRanges(tc.start, tc.end, tc.size)
			if err != nil {
				t.Fatalf("planRanges: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("range %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPlanRangesRejectsBadInput(t *testing.T) {
	if _, err := planRanges(0, 10, 0); err == nil {
		t.Error("zero segment size was accepted")
	}
	if _, err := planRanges(-1, 10, 100); err == nil {
		t.Error("negative start was accepted")
	}
}

// planRanges must cover the requested bytes exactly: no gaps, no overlaps, and
// the pieces must add up. A gap corrupts a download silently, which is the
// worst possible failure for a backup tool.
func TestPlanRangesIsExactCover(t *testing.T) {
	seed := rand.New(rand.NewSource(7))
	for range 3000 {
		segSize := int64(seed.Intn(64) + 1)
		size := int64(seed.Intn(500) + 1)
		start := int64(seed.Intn(int(size)))
		end := start + int64(seed.Intn(int(size)))
		if end >= size {
			end = size - 1
		}

		ranges, err := planRanges(start, end, segSize)
		if err != nil {
			t.Fatalf("planRanges(%d,%d,%d): %v", start, end, segSize, err)
		}

		var total int64
		next := start
		for _, r := range ranges {
			global := int64(r.Index-1)*segSize + r.Start
			if global != next {
				t.Fatalf("planRanges(%d,%d,%d): piece %+v starts at global %d, expected %d",
					start, end, segSize, r, global, next)
			}
			if r.Start < 0 || r.End >= segSize || r.End < r.Start {
				t.Fatalf("planRanges(%d,%d,%d): piece %+v is out of bounds", start, end, segSize, r)
			}
			total += r.Len()
			next = global + r.Len()
		}
		if want := end - start + 1; total != want {
			t.Fatalf("planRanges(%d,%d,%d) covers %d bytes, want %d", start, end, segSize, total, want)
		}
	}
}

func TestChunkSizeStaysInsideOneMiBWindow(t *testing.T) {
	for _, tc := range []struct{ start, end int64 }{
		{0, 0}, {0, 100}, {0, 1023}, {0, 1024}, {5, 6}, {1 << 20, 3 << 20}, {12345, 999999},
	} {
		size := ChunkSize(tc.start, tc.end)
		if size&(size-1) != 0 {
			t.Errorf("ChunkSize(%d,%d) = %d, not a power of two", tc.start, tc.end, size)
		}
		if size > 1<<20 {
			t.Errorf("ChunkSize(%d,%d) = %d, over the 1 MiB limit", tc.start, tc.end, size)
		}
		if size < 1024 {
			t.Errorf("ChunkSize(%d,%d) = %d, under the 1 KiB floor", tc.start, tc.end, size)
		}
	}
}

// The whole point of the package: reading a multi-segment file end to end must
// reproduce the original bytes exactly.
func TestReadWholeFileAcrossSegments(t *testing.T) {
	sizes := []struct {
		name          string
		size, segSize int64
	}{
		{"exact multiple of segment size", 900, 300},
		{"short final segment", 1000, 300},
		{"single segment", 200, 300},
		{"one byte over a boundary", 301, 300},
		{"one byte file", 1, 300},
		{"file smaller than a chunk", 700, 1 << 20},
		{"several megabytes over two segments", 5 << 20, 3 << 20},
	}

	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			drive := newFakeDrive(tc.size, tc.segSize)
			f, err := Open(context.Background(), drive.sourceFor, tc.size, tc.segSize, Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer f.Close()

			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, drive.data) {
				t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(drive.data))
			}
		})
	}
}

// A zero-length file has to exist and read as empty rather than erroring.
func TestReadEmptyFile(t *testing.T) {
	drive := newFakeDrive(0, 300)
	f, err := Open(context.Background(), drive.sourceFor, 0, 300, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d bytes from an empty file", len(got))
	}
}

// Seeking to every interesting offset and reading to the end must match a plain
// slice of the original. Boundary offsets are where stitching bugs live.
func TestSeekAndReadAtEveryBoundary(t *testing.T) {
	const (
		segSize = 4096
		size    = segSize*3 + 1234
	)
	drive := newFakeDrive(size, segSize)

	offsets := []int64{
		0, 1, segSize - 1, segSize, segSize + 1,
		2*segSize - 1, 2 * segSize, 2*segSize + 1,
		3*segSize - 1, 3 * segSize, 3*segSize + 1,
		size - 1, size,
	}

	for _, off := range offsets {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
			f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer f.Close()

			if got, err := f.Seek(off, io.SeekStart); err != nil || got != off {
				t.Fatalf("Seek(%d) = (%d, %v)", off, got, err)
			}
			got, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("ReadAll after seek to %d: %v", off, err)
			}
			if !bytes.Equal(got, drive.data[off:]) {
				t.Errorf("seek to %d: got %d bytes, want %d", off, len(got), int64(size)-off)
			}
		})
	}
}

// A video player scrubbing a 12 GB file lands on arbitrary offsets inside
// arbitrary segments. Every one of those reads must return the right bytes.
func TestRandomRangeReads(t *testing.T) {
	const (
		segSize = 1 << 16
		size    = segSize*5 + 7777
	)
	drive := newFakeDrive(size, segSize)
	seed := rand.New(rand.NewSource(42))

	for i := range 300 {
		start := int64(seed.Intn(size))
		length := int64(seed.Intn(size/3) + 1)
		if start+length > size {
			length = size - start
		}

		f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			t.Fatalf("Seek: %v", err)
		}

		got := make([]byte, length)
		if _, err := io.ReadFull(f, got); err != nil {
			f.Close()
			t.Fatalf("iteration %d: ReadFull(%d bytes at %d): %v", i, length, start, err)
		}
		f.Close()

		if want := drive.data[start : start+length]; !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: mismatch reading %d bytes at %d", i, length, start)
		}
	}
}

// http.ServeContent measures a file by seeking to the end and back before it
// reads anything. That must not cost a network round trip, or every WebDAV
// PROPFIND would stall.
func TestSeekDoesNoIO(t *testing.T) {
	const size, segSize = 10 << 20, 4 << 20
	drive := newFakeDrive(size, segSize)

	f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if got, _ := f.Seek(0, io.SeekEnd); got != size {
		t.Errorf("Seek(0, End) = %d, want %d", got, size)
	}
	if got, _ := f.Seek(0, io.SeekStart); got != 0 {
		t.Errorf("Seek(0, Start) = %d, want 0", got)
	}
	if n := drive.calls.Load(); n != 0 {
		t.Errorf("seeking issued %d fetches, want 0", n)
	}
}

// A short read must not pull a full readahead window. This is what keeps a
// player probing a container header from downloading megabytes.
func TestSmallReadDoesNotOverfetch(t *testing.T) {
	const size, segSize = 64 << 20, 32 << 20
	drive := newFakeDrive(size, segSize)

	f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 512)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, drive.data[:512]) {
		t.Error("content mismatch on the first 512 bytes")
	}

	// The first window is 1 MiB, so at most that much may have been fetched.
	if got := drive.bytes.Load(); got > 1<<20 {
		t.Errorf("fetched %d bytes to serve 512, want at most 1 MiB", got)
	}
}

// Reading straight through should ramp up to the wide window, so a long
// transfer is not paying one round trip per megabyte.
func TestSequentialReadRampsUpTheWindow(t *testing.T) {
	const size, segSize = 96 << 20, 64 << 20
	drive := newFakeDrive(size, segSize)

	f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	n, err := io.Copy(io.Discard, f)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != size {
		t.Fatalf("copied %d bytes, want %d", n, size)
	}

	// 96 MiB in 1 MiB chunks is 96 fetches at minimum. Anything close to that
	// means the chunk size stayed at the maximum rather than collapsing.
	if got := drive.calls.Load(); got > 130 {
		t.Errorf("used %d fetches for 96 MiB, expected roughly 96", got)
	}
}

func TestReadPropagatesSourceErrors(t *testing.T) {
	const size, segSize = 3000, 1000
	drive := newFakeDrive(size, segSize)
	drive.failSeg = 2
	drive.failAt = 0

	f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	_, err = io.ReadAll(f)
	if err == nil {
		t.Fatal("reading across a failing segment succeeded")
	}
}

// Abandoning a stream halfway — a player seeking away, a WebDAV client
// disconnecting — must release the in-flight fetches instead of leaking them.
func TestCloseCancelsInFlightFetches(t *testing.T) {
	const size, segSize = 32 << 20, 32 << 20
	drive := newFakeDrive(size, segSize)
	drive.delay = 50 * time.Millisecond

	f, err := Open(context.Background(), drive.sourceFor, size, segSize, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	buf := make([]byte, 16)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	f.Close()

	before := drive.calls.Load()
	time.Sleep(200 * time.Millisecond)
	if after := drive.calls.Load(); after > before+2 {
		t.Errorf("fetches kept starting after Close: %d then %d", before, after)
	}
}

func TestContextCancellationStopsRead(t *testing.T) {
	const size, segSize = 8 << 20, 8 << 20
	drive := newFakeDrive(size, segSize)
	drive.delay = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	f, err := Open(ctx, drive.sourceFor, size, segSize, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	cancel()
	if _, err := io.ReadAll(f); err == nil {
		t.Error("read succeeded after the context was cancelled")
	}
}

// The chunk timeout must fire rather than letting one stuck datacenter
// connection hold a download open forever.
func TestChunkTimeout(t *testing.T) {
	const size, segSize = 4 << 20, 4 << 20
	drive := newFakeDrive(size, segSize)
	drive.delay = 2 * time.Second

	f, err := Open(context.Background(), drive.sourceFor, size, segSize,
		Options{ChunkTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(f)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("read succeeded despite the chunk timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read did not give up after the chunk timeout")
	}
}

// Chunks are fetched concurrently but must be delivered in order; otherwise a
// download is silently scrambled.
func TestChunksArriveInOrderDespiteJitter(t *testing.T) {
	const size, segSize = 8 << 20, 8 << 20
	drive := newFakeDrive(size, segSize)

	jitter := &jitterSourceFor{drive: drive}
	f, err := Open(context.Background(), jitter.sourceFor, size, segSize,
		Options{Concurrency: 8, Buffers: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, drive.data) {
		t.Fatal("content was reordered by concurrent fetching")
	}
}

// jitterSourceFor makes later chunks return faster than earlier ones, which is
// the pattern that exposes a reader relying on completion order.
type jitterSourceFor struct{ drive *fakeDrive }

func (j *jitterSourceFor) sourceFor(ctx context.Context, idx int) (Source, error) {
	s, err := j.drive.sourceFor(ctx, idx)
	if err != nil {
		return nil, err
	}
	return &jitterSource{inner: s}, nil
}

type jitterSource struct{ inner Source }

func (s *jitterSource) Chunk(ctx context.Context, offset, limit int64) ([]byte, error) {
	// Earlier offsets sleep longer, so completion order is the reverse of
	// request order.
	d := time.Duration(8-min(offset/(1<<20), 7)) * time.Millisecond
	select {
	case <-time.After(d):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.inner.Chunk(ctx, offset, limit)
}
