package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// SourceFor opens a byte source for one 1-based segment index.
type SourceFor func(ctx context.Context, index int) (Source, error)

// Options tune the read pipeline. Zero values fall back to sensible defaults so
// tests can construct a reader without a config.
type Options struct {
	// Concurrency is how many chunk fetches run at once inside a window.
	Concurrency int
	// Buffers is how many completed chunks may wait to be read.
	Buffers int
	// ChunkTimeout bounds one upload.getFile call.
	ChunkTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Concurrency <= 0 {
		o.Concurrency = 6
	}
	if o.Buffers <= 0 {
		o.Buffers = 8
	}
	if o.ChunkTimeout <= 0 {
		o.ChunkTimeout = 30 * time.Second
	}
	return o
}

// Readahead ramp. A seek is as likely to be a media player sampling a few
// kilobytes of container header as it is the start of a long sequential read,
// and the two want opposite things: the first wants one small fetch, the second
// wants every connection saturated. Starting small and doubling serves both —
// a probe costs one chunk, and a real stream reaches full width within a few
// megabytes.
const (
	initialWindow = 1 << 20  // 1 MiB
	maxWindow     = 32 << 20 // beyond this, just read to the end of the segment
)

// File presents a segmented file as one seekable stream.
//
// Nothing above this type knows the file is split. Seek never performs I/O, so
// http.ServeContent's habit of seeking to the end and back to measure a file
// costs nothing; the first Read is what opens a connection.
type File struct {
	ctx       context.Context
	sourceFor SourceFor
	opts      Options

	size    int64
	segSize int64

	pos int64

	cur io.ReadCloser
	// window is how many bytes the next chunkReader is allowed to prepare.
	window int64
}

// Open builds a reader over a logical file of size bytes stored in segments of
// segSize bytes each (the last one short).
func Open(ctx context.Context, sourceFor SourceFor, size, segSize int64, opts Options) (*File, error) {
	if segSize <= 0 {
		return nil, fmt.Errorf("reader: segment size must be positive, got %d", segSize)
	}
	if size < 0 {
		return nil, fmt.Errorf("reader: negative file size %d", size)
	}
	return &File{
		ctx:       ctx,
		sourceFor: sourceFor,
		opts:      opts.withDefaults(),
		size:      size,
		segSize:   segSize,
		window:    initialWindow,
	}, nil
}

// Size is the logical size of the whole file.
func (f *File) Size() int64 { return f.size }

func (f *File) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	for {
		if f.pos >= f.size {
			return 0, io.EOF
		}
		if f.cur == nil {
			if err := f.openAt(f.pos); err != nil {
				return 0, err
			}
		}

		n, err := f.cur.Read(p)
		f.pos += int64(n)

		if errors.Is(err, io.EOF) {
			f.closeCurrent()
			// Finishing a window without error means the client is reading
			// through; widen the next one.
			if f.window < maxWindow {
				f.window *= 2
			}
			if n > 0 {
				if f.pos >= f.size {
					return n, io.EOF
				}
				return n, nil
			}
			// The window ended exactly on a boundary: loop and open the next.
			continue
		}
		if err != nil {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
		// A source that returns (0, nil) would spin the caller; try again
		// rather than propagating it.
	}
}

// Seek repositions without touching the network. The open window is discarded,
// because a seek means the bytes it was prefetching are no longer wanted.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, fmt.Errorf("reader: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("reader: seek to negative offset %d", abs)
	}

	if abs != f.pos {
		f.closeCurrent()
		// Treat a seek as a fresh access pattern: a player that jumps
		// around should not inherit a 32 MiB readahead from the previous
		// sequential run.
		f.window = initialWindow
	}
	f.pos = abs
	return abs, nil
}

// Close releases any in-flight fetches.
func (f *File) Close() error {
	f.closeCurrent()
	return nil
}

// openAt starts a window at the given offset. A window never crosses a segment
// boundary, because each segment is a separate Telegram document with its own
// location.
func (f *File) openAt(pos int64) error {
	segIdx := int(pos/f.segSize) + 1
	segStart := pos % f.segSize

	// Last readable offset inside this segment. The final segment is short,
	// so it ends where the file does rather than a full segSize in.
	base := int64(segIdx-1) * f.segSize
	segEnd := min(base+f.segSize, f.size) - 1 - base

	end := segEnd
	if f.window < maxWindow {
		if windowed := segStart + f.window - 1; windowed < end {
			end = windowed
		}
	}
	if end < segStart {
		return io.EOF
	}

	src, err := f.sourceFor(f.ctx, segIdx)
	if err != nil {
		return fmt.Errorf("open segment %d: %w", segIdx, err)
	}

	f.cur = newChunkReader(f.ctx, src, segStart, end,
		f.opts.Concurrency, f.opts.Buffers, f.opts.ChunkTimeout)
	return nil
}

func (f *File) closeCurrent() {
	if f.cur != nil {
		_ = f.cur.Close()
		f.cur = nil
	}
}

// Ranges reports how a byte range maps onto segments. The API layer uses it to
// explain a file's layout in the details panel, and the tests use it to check
// the mapping directly.
func Ranges(start, end, segSize int64) ([]SegRange, error) {
	return planRanges(start, end, segSize)
}
