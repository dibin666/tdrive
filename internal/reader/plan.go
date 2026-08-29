// Package reader turns a file that Telegram stores as several documents back
// into one continuous stream of bytes.
//
// Everything above this package — the REST API, the WebDAV filesystem, the
// video player in the browser — sees a single file of a single size. Segments
// exist only here and in internal/uploader.
//
// There are two layers. SegmentReader maps a byte range of the logical file
// onto ranges inside individual segments and stitches the results together.
// Inside each segment, chunkReader fetches 1 MiB pieces several at a time and
// releases them in order, which is where the throughput comes from: the
// reference implementation walks a single connection through 512 KiB chunks
// sequentially, so its download speed is one round trip at a time.
package reader

import "fmt"

// SegRange is a byte range inside one segment. Index is 1-based, matching both
// the segments table and the #seg_i_n caption tag, so a range can be compared
// against either without converting.
type SegRange struct {
	Index int
	// Start and End are inclusive offsets within the segment.
	Start int64
	End   int64
}

// Len is the number of bytes this range covers.
func (r SegRange) Len() int64 { return r.End - r.Start + 1 }

// planRanges maps the inclusive global range [start, end] onto per-segment
// ranges.
//
// It assumes every segment except the last is exactly segSize bytes, which the
// uploader guarantees: it splits on fixed boundaries rather than wherever a
// read happened to end. That assumption is what makes this arithmetic instead
// of a lookup, so a seek into the middle of a 12 GB file costs nothing.
func planRanges(start, end, segSize int64) ([]SegRange, error) {
	switch {
	case segSize <= 0:
		return nil, fmt.Errorf("reader: segment size must be positive, got %d", segSize)
	case start < 0:
		return nil, fmt.Errorf("reader: negative start offset %d", start)
	case end < start:
		// An empty range is legitimate: a zero-length file, or a client
		// asking for zero bytes.
		return nil, nil
	}

	first := start / segSize
	last := end / segSize

	out := make([]SegRange, 0, last-first+1)
	for s := first; s <= last; s++ {
		base := s * segSize
		segStart := start - base
		if segStart < 0 {
			segStart = 0
		}
		segEnd := end - base
		if segEnd > segSize-1 {
			segEnd = segSize - 1
		}
		out = append(out, SegRange{Index: int(s) + 1, Start: segStart, End: segEnd})
	}
	return out, nil
}

// chunkPlan describes how one segment range is broken into aligned fetches.
//
// upload.getFile will not serve a read that straddles a 1 MiB boundary, so
// reads are aligned down to a power-of-two chunk size and the unwanted head and
// tail are trimmed after the fact.
type chunkPlan struct {
	chunkSize int64
	// base is the aligned offset of the first chunk.
	base int64
	// count is how many chunks cover the range.
	count int
	// leftCut is how many bytes to drop from the front of the first chunk.
	leftCut int64
	// length is the total number of bytes the caller asked for.
	length int64
}

// planChunks lays out the aligned fetches covering the inclusive range
// [start, end] of one segment.
func planChunks(start, end, chunkSize int64) chunkPlan {
	base := start - start%chunkSize
	count := int((end-base)/chunkSize) + 1
	return chunkPlan{
		chunkSize: chunkSize,
		base:      base,
		count:     count,
		leftCut:   start - base,
		length:    end - start + 1,
	}
}
