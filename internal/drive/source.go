package drive

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/reader"
)

// Telegram file references are anti-hotlinking tokens that go stale after
// roughly an hour. Reading with a stale one fails, which is recoverable:
// re-read the message, take the fresh reference, retry.
//
// The awkward part is that a stale reference is discovered by every concurrent
// chunk fetch at once. Without coordination, a six-way parallel read turns one
// expiry into six identical getMessages calls — exactly the kind of burst that
// earns a FLOOD_WAIT. singleflight collapses them into one.

type refEntry struct {
	ref     []byte
	dc      int
	expires time.Time
}

type refCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]refEntry
	group   singleflight.Group
}

func newRefCache(ttl time.Duration) *refCache {
	return &refCache{ttl: ttl, entries: make(map[string]refEntry)}
}

func refKey(fileID string, idx int) string { return fileID + ":" + strconv.Itoa(idx) }

func (c *refCache) get(key string) (refEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return refEntry{}, false
	}
	return e, true
}

func (c *refCache) put(key string, ref []byte, dc int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = refEntry{ref: ref, dc: dc, expires: time.Now().Add(c.ttl)}
}

func (c *refCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Forget clears every cached reference for a file, used when its messages are
// deleted or their captions rewritten.
func (c *refCache) Forget(fileID string) {
	prefix := fileID + ":"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// segState is what a source has learned about its segment since the stream
// opened: a refreshed file reference, a corrected datacenter, or both.
//
// It is held on the source rather than only in refCache because correctness
// must not depend on a cache. The chunks of one read run concurrently and would
// otherwise each rediscover the same expiry or migration, and a cache with a
// short TTL would make them rediscover it again on the next chunk.
type segState struct {
	ref []byte
	dc  int
}

// segmentSource reads one segment's bytes, transparently repairing an expired
// file reference or a datacenter migration.
type segmentSource struct {
	svc     *Service
	file    database.File
	seg     database.Segment
	channel database.Channel
	key     string

	state atomic.Pointer[segState]
}

func (s *segmentSource) doc(ref []byte, dc int) StoredDoc {
	return StoredDoc{
		MsgID:         s.seg.TGMsgID,
		DocID:         s.seg.TGDocID,
		AccessHash:    s.seg.AccessHash,
		DCID:          dc,
		FileReference: ref,
		Size:          s.seg.Size,
	}
}

func (s *segmentSource) Chunk(ctx context.Context, offset, limit int64) ([]byte, error) {
	fileRef, dc := s.current()

	data, err := s.svc.tg.ReadChunk(ctx, ref(s.channel), s.doc(fileRef, dc), offset, limit)
	if err == nil {
		return data, nil
	}

	// A migration says exactly where the document lives; remember it so the
	// rest of this download and every later one go straight there.
	if newDC, ok := s.svc.tg.MigratedDC(err); ok && newDC != dc {
		s.remember(fileRef, newDC)
		s.svc.persistDC(ctx, s.file.ID, s.seg.Index, newDC)
		return s.svc.tg.ReadChunk(ctx, ref(s.channel), s.doc(fileRef, newDC), offset, limit)
	}

	if !s.svc.tg.IsReferenceExpired(err) {
		return nil, err
	}

	fresh, freshDC, refreshErr := s.refresh(ctx)
	if refreshErr != nil {
		return nil, fmt.Errorf("refresh file reference for segment %d: %w", s.seg.Index, refreshErr)
	}
	return s.svc.tg.ReadChunk(ctx, ref(s.channel), s.doc(fresh, freshDC), offset, limit)
}

// current returns the best-known reference and datacenter: what this stream
// has already learned, then the shared cache, then the stored row.
func (s *segmentSource) current() ([]byte, int) {
	if st := s.state.Load(); st != nil {
		return st.ref, st.dc
	}
	if e, ok := s.svc.refs.get(s.key); ok {
		return e.ref, e.dc
	}
	return s.seg.FileReference, s.seg.DCID
}

// remember records a repair for the rest of this stream and, through the
// cache, for streams that start soon after.
func (s *segmentSource) remember(fileRef []byte, dc int) {
	s.state.Store(&segState{ref: fileRef, dc: dc})
	s.svc.refs.put(s.key, fileRef, dc)
}

// refresh re-reads the message backing this segment. Concurrent callers share
// one round trip.
func (s *segmentSource) refresh(ctx context.Context) ([]byte, int, error) {
	s.svc.refs.drop(s.key)
	s.state.Store(nil)

	res, err, _ := s.svc.refs.group.Do(s.key, func() (any, error) {
		doc, err := s.svc.tg.RefreshDoc(ctx, ref(s.channel), s.seg.TGMsgID)
		if err != nil {
			return nil, err
		}

		s.remember(doc.FileReference, doc.DCID)
		// Persist so a restart does not have to rediscover it.
		if err := s.svc.db.RefreshFileReference(ctx, s.file.ID, s.seg.Index, doc.FileReference); err != nil {
			s.svc.log.Warn("could not persist a refreshed file reference",
				zap.String("file", s.file.ID), zap.Int("segment", s.seg.Index), zap.Error(err))
		}
		s.svc.persistDC(ctx, s.file.ID, s.seg.Index, doc.DCID)
		return doc, nil
	})
	if err != nil {
		return nil, 0, err
	}

	doc := res.(StoredDoc)
	return doc.FileReference, doc.DCID, nil
}

// persistDC records where a document really lives, so later reads skip the
// migration round trip.
func (s *Service) persistDC(ctx context.Context, fileID string, idx, dc int) {
	if dc <= 0 {
		return
	}
	if err := s.db.SetSegmentDC(ctx, fileID, idx, dc); err != nil {
		s.log.Warn("could not persist a segment's datacenter",
			zap.String("file", fileID), zap.Int("segment", idx), zap.Error(err))
	}
}

// OpenFile returns a seekable stream over a whole logical file. Callers get one
// continuous file no matter how many Telegram documents back it.
func (s *Service) OpenFile(ctx context.Context, f database.File) (*reader.File, error) {
	segs, err := s.db.Segments(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 && f.Size > 0 {
		return nil, fmt.Errorf("file %q has no stored segments", f.Name)
	}

	channel, err := s.channelFor(ctx, f.ChannelID)
	if err != nil {
		return nil, err
	}

	byIndex := make(map[int]database.Segment, len(segs))
	for _, seg := range segs {
		byIndex[seg.Index] = seg
	}

	sourceFor := func(_ context.Context, idx int) (reader.Source, error) {
		seg, ok := byIndex[idx]
		if !ok {
			// A missing segment is why files get marked broken; say so
			// plainly rather than returning zeros.
			return nil, fmt.Errorf("file %q is missing segment %d of %d",
				f.Name, idx, f.SegmentCount)
		}
		return &segmentSource{
			svc:     s,
			file:    f,
			seg:     seg,
			channel: channel,
			key:     refKey(f.ID, idx),
		}, nil
	}

	settings := s.cfg.RuntimeSettings()
	return reader.Open(ctx, sourceFor, f.Size, f.SegmentSize, reader.Options{
		Concurrency:  settings.StreamConcurrency,
		Buffers:      s.cfg.Stream.Buffers,
		ChunkTimeout: s.cfg.Stream.ChunkTimeout,
	})
}
