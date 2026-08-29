package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// Source fetches bytes from one stored segment. Splitting this out keeps the
// stitching logic testable without a Telegram connection.
type Source interface {
	// Chunk returns up to limit bytes starting at offset. offset and limit
	// are already aligned so that the read stays inside one 1 MiB window.
	Chunk(ctx context.Context, offset, limit int64) ([]byte, error)
}

// chunkReader streams one range of one segment, fetching several chunks at a
// time while handing them to the caller strictly in order.
//
// The ordering is done with a queue of single-slot result channels rather than
// by fetching in fixed batches. A batch design stalls on its slowest member —
// every other connection sits idle until the straggler lands — whereas a
// continuous queue lets a fetch start the moment a slot frees up. On a link
// where one datacenter round trip occasionally takes much longer than the
// others, that difference is most of the throughput.
type chunkReader struct {
	ctx    context.Context
	cancel context.CancelFunc

	futures chan chan chunkResult
	cur     []byte
	remain  int64

	closeOnce sync.Once
}

type chunkResult struct {
	data []byte
	err  error
}

// newChunkReader starts fetching immediately so that the first Read does not
// pay the full round trip.
func newChunkReader(
	ctx context.Context,
	src Source,
	start, end int64,
	concurrency, buffers int,
	chunkTimeout time.Duration,
) *chunkReader {
	chunkSize := ChunkSize(start, end)
	plan := planChunks(start, end, chunkSize)

	ctx, cancel := context.WithCancel(ctx)
	r := &chunkReader{
		ctx:     ctx,
		cancel:  cancel,
		futures: make(chan chan chunkResult, max(buffers, 1)),
		remain:  plan.length,
	}

	go r.pump(src, plan, concurrency, chunkTimeout)
	return r
}

// pump issues fetches in order, bounded by a semaphore, and publishes each
// one's future before starting the next. Publishing first is what preserves
// ordering: the reader drains futures in the same sequence they were queued.
func (r *chunkReader) pump(src Source, plan chunkPlan, concurrency int, timeout time.Duration) {
	defer close(r.futures)

	sem := semaphore.NewWeighted(int64(max(concurrency, 1)))
	var wg sync.WaitGroup
	defer wg.Wait()

	for i := range plan.count {
		if err := sem.Acquire(r.ctx, 1); err != nil {
			return
		}

		future := make(chan chunkResult, 1)
		select {
		case r.futures <- future:
		case <-r.ctx.Done():
			sem.Release(1)
			return
		}

		wg.Add(1)
		go func(idx int, out chan chunkResult) {
			defer wg.Done()
			defer sem.Release(1)

			// A per-chunk timeout keeps one wedged datacenter connection
			// from stalling a download forever; the pool has other
			// connections that would happily serve the retry.
			fetchCtx, cancel := context.WithTimeout(r.ctx, timeout)
			defer cancel()

			offset := plan.base + int64(idx)*plan.chunkSize
			data, err := src.Chunk(fetchCtx, offset, plan.chunkSize)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && r.ctx.Err() == nil {
					err = fmt.Errorf("chunk at offset %d timed out after %s", offset, timeout)
				}
				out <- chunkResult{err: err}
				// One failed chunk makes the rest of the stream useless;
				// cancelling releases the other connections at once.
				r.cancel()
				return
			}

			if idx == 0 && plan.leftCut > 0 {
				if plan.leftCut >= int64(len(data)) {
					out <- chunkResult{err: io.ErrUnexpectedEOF}
					r.cancel()
					return
				}
				data = data[plan.leftCut:]
			}
			out <- chunkResult{data: data}
		}(i, future)
	}
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		return 0, io.EOF
	}

	for len(r.cur) == 0 {
		select {
		case future, ok := <-r.futures:
			if !ok {
				// The producer finished without covering the whole range,
				// which means Telegram returned a short document.
				return 0, io.ErrUnexpectedEOF
			}
			select {
			case res := <-future:
				if res.err != nil {
					return 0, res.err
				}
				if len(res.data) == 0 {
					return 0, io.ErrUnexpectedEOF
				}
				r.cur = res.data
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			}
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}

	// The tail of the last chunk is trimmed here rather than in the fetcher,
	// because only the reader knows how many bytes are still owed.
	if int64(len(r.cur)) > r.remain {
		r.cur = r.cur[:r.remain]
	}

	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	r.remain -= int64(n)
	if r.remain <= 0 {
		return n, io.EOF
	}
	return n, nil
}

// Close stops in-flight fetches. Abandoning a stream mid-file is the normal
// case for a video player, so this has to release connections promptly.
func (r *chunkReader) Close() error {
	r.closeOnce.Do(r.cancel)
	return nil
}

// ChunkSize picks the read granularity for an inclusive range.
//
// 1 MiB is Telegram's per-call maximum and the right unit for streaming a whole
// file. A short range — a player probing a container header, or a WebDAV client
// reading a few bytes — gets a smaller power of two so it does not pull a
// megabyte to satisfy a kilobyte. Powers of two also keep every read inside a
// single 1 MiB window, which upload.getFile requires.
func ChunkSize(start, end int64) int64 {
	size := int64(1024 * 1024)
	for size > 1024 && size > (end-start) {
		size /= 2
	}
	return size
}
